//! FAST marginfi liquidation executor — the streaming rewrite.
//!
//! The polling executor (liq_executor) reacts in ~150ms because it re-fetches
//! account state (getMultipleAccounts ~40ms) and sim-gates (up to 5×45ms) on the
//! hot path. This one removes BOTH, and pre-builds the fire so the hot path is
//! sign-and-send only:
//!
//!   • STATE is streamed. A Yellowstone gRPC (Triton Dragon's Mouth) subscription
//!     to the watch-set accounts + banks + oracles keeps the loan book in RAM —
//!     no hot-path fetch.
//!   • PRICES: streamed on-chain oracles (fresh-gated per bank, stale dropped like
//!     the chain does) blended with Pyth Lazer (the ms trigger).
//!   • PRE-ARM: a background thread continuously builds+caches a fire tx for the
//!     handful of accounts closest to crossing (the expensive Jupiter quote +
//!     compile happens OFF the hot path).
//!   • HOT PATH: on a Lazer tick, recompute health for ONLY the armed set (in-RAM,
//!     ~µs), and on a cross refresh the cached tx's blockhash, sign, and send to
//!     Amsterdam Jito with NO sim. Decision → submit ≈ 1ms; profit-or-revert is
//!     the safety.
//!
//! Usage: HELIUS_RPC=<url> GRPC_ENDPOINT=<triton-url+token> GRPC_X_TOKEN=<tok>
//!        PYTH_LAZER_TOKEN=<tok> [KEYPAIR_PATH=…] [DRY_RUN=1] [MIN_PROFIT_USD=0.02]
//!        [WATCH_RATIO=0.90] [ARM_RATIO=0.97] [ARM_MAX=40] [RUN_DIR=runs/stream]
//!        cargo run --release --bin liq_stream_executor

use anyhow::Result;
use arb_engine::liq_fire::{self, FireCandidate};
use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::pyth_accumulator::{spawn_hermes_cache, HermesCache};
use arb_engine::{jito, lazer, pyth, pyth_crank};
use futures::StreamExt;
use solana_hash::Hash;
use solana_instruction::AccountMeta;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::sync::atomic::{AtomicU64, Ordering};
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest, SubscribeRequestFilterAccounts,
};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const SOL_MINT: &str = "So11111111111111111111111111111111111111112";
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";

fn now() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }
fn now_us() -> u128 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_micros() }
fn is_debt_mint(m: &Pubkey) -> bool {
    let s = m.to_string();
    s == USDC_MINT || s == USDT_MINT || s == SOL_MINT
}

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for a in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(300 << a));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]})) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(b) = acc.get("data").and_then(b64) { out.insert(chunk[i], b); }
        }
    }
    out
}
/// Batch-resolve each mint's owning token program (SPL Token vs Token-2022) in
/// one round-trip per 100. Done ONCE at startup so the hot/arm paths never RPC.
fn get_owners(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Pubkey> {
    let mut out = HashMap::new();
    for chunk in keys.chunks(100) {
        let strs: Vec<String> = chunk.iter().map(|k| k.to_string()).collect();
        let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[strs, {"encoding":"base64"}]})) else { continue };
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(o) = acc.get("owner").and_then(|x| x.as_str()).and_then(|s| s.parse().ok()) { out.insert(chunk[i], o); }
        }
    }
    out
}
fn get_slot(endpoint: &str) -> u64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"processed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0)
}
fn latest_blockhash(endpoint: &str) -> Option<Hash> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))?;
    Hash::from_str(v["result"]["value"]["blockhash"].as_str()?).ok()
}
/// Simulate a signed tx (verification before real sends). Returns (ok, err+log, units).
fn simulate_tx(endpoint: &str, b64tx: &str) -> (bool, Option<String>, Option<u64>) {
    let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]})) else {
        return (false, Some("rpc call failed".into()), None) };
    let val = &v["result"]["value"];
    let units = val.get("unitsConsumed").and_then(|u| u.as_u64());
    let err = val.get("err");
    if err.is_none() || err == Some(&serde_json::Value::Null) { return (true, None, units); }
    let last_logs: String = val.get("logs").and_then(|l| l.as_array())
        .map(|a| a.iter().rev().take(3).filter_map(|x| x.as_str()).collect::<Vec<_>>().join(" | ")).unwrap_or_default();
    (false, Some(format!("{} :: {last_logs}", err.unwrap())), units)
}
/// Pre-arm sim gate: cache a fire only if it simulates clean OR fails 6068 (chain
/// says healthy — the obs wiring is correct, it's just not liquidatable yet, so it
/// WILL fire cleanly on a cross). Everything else (6051 wiring, swap errors, …) is
/// rejected so the hot path never blasts a guaranteed-revert tx.
fn sim_cacheable(endpoint: &str, tx: &VersionedTransaction) -> (bool, String) {
    use base64::Engine;
    let b64tx = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx).unwrap());
    let (ok, err, _) = simulate_tx(endpoint, &b64tx);
    if ok { return (true, "ok".into()); }
    let e = err.unwrap_or_default();
    if e.contains("6068") { return (true, "not-yet(6068)".into()); }
    (false, e)
}

/// One getProgramAccounts scan of the marginfi group → every borrower (accounts
/// with a liability) + each one's active-bank obs list. Used at startup AND by the
/// periodic re-scan (balances drift slowly, so full-book state stays fresh enough
/// to catch a price crash — which moves PRICES, not balances).
fn scan_book(endpoint: &str) -> (Vec<(Pubkey, MarginfiAccount)>, HashMap<Pubkey, Vec<Pubkey>>) {
    let mut accts: Vec<(Pubkey, MarginfiAccount)> = Vec::new();
    let mut obs: HashMap<Pubkey, Vec<Pubkey>> = HashMap::new();
    let Some(resp) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})) else { return (accts, obs) };
    for e in resp["result"].as_array().cloned().unwrap_or_default().iter() {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse().ok()) else { continue };
        let Some(raw) = b64(&e["account"]["data"]) else { continue };
        let Some(a) = MarginfiAccount::decode(&raw) else { continue };
        if !a.balances.iter().any(|b| b.liability_shares > 0.0) { continue; }
        obs.insert(pk, liq::active_bank_pks(&raw));
        accts.push((pk, a));
    }
    (accts, obs)
}

/// The live loan book — written by the gRPC task, read by the arm/fire loops.
#[derive(Default)]
struct LiveState {
    accounts: HashMap<Pubkey, MarginfiAccount>,
    banks: BankMap,
    oracle_of: HashMap<Pubkey, Pubkey>,      // bank_pk → oracle_pk
    oracle_raw: HashMap<Pubkey, Vec<u8>>,    // oracle_pk → latest raw bytes (fresh-decoded at use)
    obs_banks: HashMap<Pubkey, Vec<Pubkey>>, // account → ALL active-flag banks (for the obs list)
}

impl LiveState {
    /// On-chain baseline PriceMap (bank → USD) — stale oracles DROPPED per bank
    /// (decode_oracle_price_fresh), exactly matching the chain's staleness gate.
    /// A dropped bank reads as `missing` in health and is never fired on.
    fn fresh_base(&self, slot: u64, default_stale: u64) -> PriceMap {
        self.oracle_of.iter().filter_map(|(bk, oc)| {
            let max_age = self.banks.get(bk).map(|b| b.oracle_max_age).unwrap_or(0);
            let max_stale = liq::max_stale_slots_for(max_age, default_stale);
            let usd = liq::decode_oracle_price_fresh(self.oracle_raw.get(oc)?, slot, max_stale)?;
            Some((*bk, usd))
        }).collect()
    }
}

/// A pre-built fire kept hot for an armed account. Blockhash is refreshed at fire
/// time (set_recent_blockhash), so only the Jupiter quote ages — hence rebuild.
struct CachedFire { tx: VersionedTransaction, seize: u64, quoted_out: u64, built: Instant, asset_bank: Pubkey, crank: bool }

#[tokio::main]
async fn main() -> Result<()> {
    let _ = rustls::crypto::ring::default_provider().install_default();
    let _ = dotenvy::dotenv();
    let endpoint = std::env::var("HELIUS_RPC").expect("HELIUS_RPC");
    let grpc_ep = std::env::var("GRPC_ENDPOINT").expect("GRPC_ENDPOINT");
    let grpc_tok = std::env::var("GRPC_X_TOKEN").expect("GRPC_X_TOKEN");
    let dry_run = std::env::var("DRY_RUN").map(|v| v != "0").unwrap_or(true);
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(5.0);
    // Fire only when CLEARLY underwater (liab/assets ≥ 1 + margin), not borderline —
    // aligns our Lazer flag with what Pyth/marginfi actually judge, so fired bundles
    // land instead of reverting on healthy-at-Pyth phantoms. Free (one comparison).
    let underwater_margin: f64 = std::env::var("MIN_UNDERWATER_MARGIN").ok().and_then(|s| s.parse().ok()).unwrap_or(0.01);
    let verify_arm = std::env::var("VERIFY_ARM").map(|v| v != "0").unwrap_or(true); // sim-gate in pre-arm
    let arm_max: usize = std::env::var("ARM_MAX").ok().and_then(|s| s.parse().ok()).unwrap_or(10);
    let arm_rebuild = Duration::from_secs(std::env::var("ARM_REBUILD_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let quote_gap_ms: u64 = std::env::var("QUOTE_GAP_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1200);
    let synth = std::env::var("ARM_SYNTH").is_ok(); // measurement-only: skip Jupiter, cache placeholders
    let default_stale: u64 = std::env::var("MAX_SB_STALE_SLOTS").ok().and_then(|s| s.parse().ok()).unwrap_or(liq::DEFAULT_MAX_SB_STALE_SLOTS);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs/stream".into());
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let slippage_bps: u32 = std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100);
    let tip_sol: f64 = std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002);
    let _ = std::fs::create_dir_all(&run_dir);

    let kp: Option<Arc<Keypair>> = std::env::var("KEYPAIR_PATH").ok().and_then(|p| std::fs::read_to_string(p).ok())
        .and_then(|s| serde_json::from_str::<Vec<u8>>(&s).ok()).and_then(|b| Keypair::try_from(&b[..]).ok()).map(Arc::new);
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str("DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak").unwrap());

    // ── ONE-TIME heavy scan: the FULL book (every borrower) + banks + oracles ──
    // We keep ALL borrowers, not a near-threshold slice — a price crash liquidates
    // accounts that were HEALTHY at startup, so a static watch-set is blind to them.
    eprintln!("[stream-exec] initial scan (one getProgramAccounts) …");
    let slot0 = get_slot(&endpoint);
    let (accts, obs_banks_map) = scan_book(&endpoint);
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of: HashMap<Pubkey, Pubkey> = HashMap::new();
    for (pk, raw) in &get_multiple(&endpoint, &bank_pks) { if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); } }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_set: HashSet<Pubkey> = oracle_pks.iter().copied().collect();
    let oracle_raw: HashMap<Pubkey, Vec<u8>> = get_multiple(&endpoint, &oracle_pks);
    // Pre-resolve every bank mint's token program ONCE (SPL vs Token-2022) so the
    // arm/hot paths build candidates with zero RPC.
    let all_mints: Vec<Pubkey> = banks.values().map(|b| b.mint).collect::<HashSet<_>>().into_iter().collect();
    let mint_tp: Arc<HashMap<Pubkey, Pubkey>> = Arc::new(get_owners(&endpoint, &all_mints));
    eprintln!("[stream-exec] resolved {} mint token-programs", mint_tp.len());
    // Crank metadata: banks whose oracle is a crankable Pyth shard-0 sponsored feed,
    // + each bank's feed id. Lets us POST a fresh price then liquidate atomically —
    // the edge for accounts underwater at the true price but healthy at the stale
    // on-chain price (they'd otherwise 6068).
    let mut feed_of: HashMap<Pubkey, [u8; 32]> = HashMap::new();
    let mut crankable: HashSet<Pubkey> = HashSet::new();
    for (bank, oracle) in &oracle_of {
        if let Some((fid, _, _)) = oracle_raw.get(oracle).and_then(|r| liq::decode_price_update_v2(r)) {
            feed_of.insert(*bank, fid);
            if pyth_crank::sponsored_feed(0, &fid) == *oracle { crankable.insert(*bank); }
        }
    }
    let feed_of = Arc::new(feed_of);
    let crankable = Arc::new(crankable);
    eprintln!("[stream-exec] {} crankable banks (Pyth sponsored feeds)", crankable.len());

    let state = Arc::new(RwLock::new(LiveState {
        accounts: accts.iter().cloned().collect(), banks, oracle_of, oracle_raw, obs_banks: obs_banks_map,
    }));

    let n_banks = state.read().unwrap().banks.len();
    let n_book = accts.len();
    eprintln!("[stream-exec] FULL BOOK: {n_book} borrowers, {n_banks} banks, {} oracles @ slot {slot0}", oracle_pks.len());

    // ── gRPC subscription: banks + oracles ONLY (fresh prices). Account STATE
    // comes from the periodic re-scan below — 81k account subs would be too many,
    // and balances drift slowly vs the prices that trigger a crash. ──
    {
        let (state, grpc_ep, grpc_tok, oracle_set) = (state.clone(), grpc_ep.clone(), grpc_tok.clone(), oracle_set.clone());
        let mut sub: Vec<String> = bank_pks.iter().map(|p| p.to_string()).collect();
        sub.extend(oracle_pks.iter().map(|p| p.to_string()));
        tokio::spawn(async move {
            loop {
                if let Err(e) = run_stream(&grpc_ep, &grpc_tok, &sub, &oracle_set, &state).await {
                    eprintln!("[stream-exec] gRPC dropped ({e}); reconnecting in 2s");
                    tokio::time::sleep(Duration::from_secs(2)).await;
                }
            }
        });
    }

    // ── Re-scan thread: refresh the FULL book's account state periodically so we
    // stay current with new borrowers / balance changes (a crash moves prices, not
    // balances, so this cadence is fine to catch it). ──
    {
        let (state, endpoint) = (state.clone(), endpoint.clone());
        let rescan_secs: u64 = std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(90);
        std::thread::spawn(move || loop {
            std::thread::sleep(Duration::from_secs(rescan_secs));
            let (accts, obs) = scan_book(&endpoint);
            if accts.is_empty() { continue; }
            let n = accts.len();
            { let mut w = state.write().unwrap(); w.accounts = accts.into_iter().collect(); w.obs_banks = obs; }
            if std::env::var("ARM_DEBUG").is_ok() { eprintln!("[rescan] refreshed full book: {n} borrowers"); }
        });
    }

    // ── Pyth Lazer prices (fast trigger) ──
    let lazer_table = pyth::new_table();
    let lazer_map = lazer::mint_feed_map();
    if let Ok(tok) = std::env::var("PYTH_LAZER_TOKEN") {
        lazer::spawn_lazer_thread(tok, lazer::arm_feed_ids(), lazer_table.clone());
        eprintln!("[stream-exec] Pyth Lazer trigger ENABLED");
    }

    // ── current slot + blockhash hot (light RPC, off hot path) ──
    let cur_slot = Arc::new(AtomicU64::new(slot0));
    let blockhash = Arc::new(RwLock::new(latest_blockhash(&endpoint).unwrap_or_default()));
    {
        let (cs, bh, ep) = (cur_slot.clone(), blockhash.clone(), endpoint.clone());
        std::thread::spawn(move || loop {
            let s = get_slot(&ep); if s > 0 { cs.store(s, Ordering::Relaxed); }
            if let Some(h) = latest_blockhash(&ep) { *bh.write().unwrap() = h; }
            std::thread::sleep(Duration::from_secs(2));
        });
    }

    // Two tip destinations: crank fires go out as a Jito BUNDLE (tip → Jito tip
    // account); plain Sender fires tip a HELIUS wallet (the /fast endpoint requires it).
    let block_engine = jito::default_block_engine();
    let jito_tip = jito::get_tip_accounts(&block_engine).ok().and_then(|v| v.into_iter().next());
    let helius_tip = Some(Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT")
        .unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap());
    let crank_on = std::env::var("CRANK").map(|s| s != "0").unwrap_or(true);
    let max_blob_age = Duration::from_millis(std::env::var("MAX_BLOB_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(2000));
    // Hermes cache: keep fresh signed price blobs hot for every crankable feed.
    let hermes: Arc<HermesCache> = {
        let url = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
        let h = spawn_hermes_cache(url, vec![], Duration::from_millis(400));
        let hex: Vec<String> = crankable.iter().filter_map(|b| feed_of.get(b))
            .map(|f| f.iter().map(|x| format!("{x:02x}")).collect::<String>()).collect::<HashSet<_>>().into_iter().collect();
        eprintln!("[stream-exec] Hermes cache tracking {} crankable feeds{}", hex.len(), if crank_on { "" } else { " (CRANK disabled)" });
        h.set_feeds(hex);
        Arc::new(h)
    };
    let sim_only = std::env::var("SIM_ONLY").is_ok(); // verify the fire simulates before real sends
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let cache: Arc<RwLock<HashMap<Pubkey, CachedFire>>> = Arc::new(RwLock::new(HashMap::new()));
    // Trigger index: per dominant-collateral BANK, accounts sorted DESC by the
    // collateral price at which they become liquidatable. The hot path binary-searches
    // this against the live price — O(log n) full-book detection, no per-tick recompute.
    let triggers: Arc<RwLock<HashMap<Pubkey, Vec<(f64, Pubkey)>>>> = Arc::new(RwLock::new(HashMap::new()));
    let arm_band: f64 = std::env::var("ARM_BAND").ok().and_then(|s| s.parse().ok()).unwrap_or(0.03);

    // ── TRIGGER-INDEX + PRE-ARM thread: over the FULL book, compute each account's
    // liquidation trigger price (2-eval perturbation), build the sorted index, and
    // pre-build fire txs for the arm-band (accounts within ARM_BAND of crossing).
    // OFF the hot path. ──
    {
        let (state, cache, mint_tp, blockhash, cur_slot, endpoint) =
            (state.clone(), cache.clone(), mint_tp.clone(), blockhash.clone(), cur_slot.clone(), endpoint.clone());
        let (lazer_table, lazer_map, triggers) = (lazer_table.clone(), lazer_map.clone(), triggers.clone());
        let crankable_c = crankable.clone();
        std::thread::spawn(move || loop {
            let slot = cur_slot.load(Ordering::Relaxed);
            // Compute the trigger index + arm candidates over the ENTIRE book.
            let (idx, mut ranked, n_book, n_now): (HashMap<Pubkey, Vec<(f64, Pubkey)>>, Vec<(Pubkey, f64, Pubkey)>, usize, usize) = {
                let s = state.read().unwrap();
                let base = s.fresh_base(slot, default_stale);
                let (mut m, _) = lazer::blend(&s.banks, &base, &lazer_table, &lazer_map); // owned; perturb+restore one entry per acct (no per-acct clone)
                let mut idx: HashMap<Pubkey, Vec<(f64, Pubkey)>> = HashMap::new();
                let mut arm: Vec<(Pubkey, f64, Pubkey)> = Vec::new(); // (account, ratio, dominant mint)
                let mut now_liq = 0usize;
                for (pk, a) in s.accounts.iter() {
                    // Dominant collateral bank = the balance with the largest USD value.
                    let dom = a.balances.iter().filter(|b| b.asset_shares > 0.0).filter_map(|b| {
                        let bk = s.banks.get(&b.bank_pk)?; let p = m.get(&b.bank_pk)?;
                        Some((b.bank_pk, b.asset_shares * bk.asset_share_value * p))
                    }).max_by(|x, y| x.1.partial_cmp(&y.1).unwrap_or(std::cmp::Ordering::Equal));
                    let Some((dom_bank, _)) = dom else { continue };
                    let dom_mint = match s.banks.get(&dom_bank) { Some(bk) => bk.mint, None => continue };
                    let h0 = liq::maintenance_health(a, &s.banks, &m);
                    if h0.missing != 0 || h0.health.weighted_assets < min_collateral { continue; }
                    if h0.health.ratio() >= 1.0 + underwater_margin {
                        now_liq += 1; arm.push((*pk, h0.health.ratio(), dom_mint)); continue; // already liquidatable
                    }
                    let p0 = match m.get(&dom_bank) { Some(p) => *p, None => continue };
                    m.insert(dom_bank, p0 * 0.9);                 // perturb dominant collateral price
                    let h1 = liq::maintenance_health(a, &s.banks, &m);
                    m.insert(dom_bank, p0);                       // restore
                    let slope = (h1.health.weighted_assets - h0.health.weighted_assets) / (p0 * 0.9 - p0);
                    if slope <= 0.0 { continue; }
                    let trigger = p0 + (h0.health.weighted_liabilities - h0.health.weighted_assets) / slope;
                    if !trigger.is_finite() || trigger <= 0.0 || trigger >= p0 { continue; }
                    idx.entry(dom_bank).or_default().push((trigger, *pk));
                    if trigger >= p0 * (1.0 - arm_band) { arm.push((*pk, h0.health.ratio(), dom_mint)); } // arm band
                }
                for list in idx.values_mut() { list.sort_by(|x, y| y.0.partial_cmp(&x.0).unwrap_or(std::cmp::Ordering::Equal)); }
                let nb = s.accounts.len();
                (idx, arm, nb, now_liq)
            };
            let n_trig: usize = idx.values().map(|v| v.len()).sum();
            *triggers.write().unwrap() = idx; // publish the fresh index for the hot path

            // Pre-arm the candidates: dedupe direct-DEX by asset, cap, build+sim-gate.
            ranked.sort_by(|a, b| b.1.partial_cmp(&a.1).unwrap_or(std::cmp::Ordering::Equal));
            let usdc = Pubkey::from_str(USDC_MINT).unwrap();
            let mut seen_asset: HashSet<Pubkey> = HashSet::new();
            ranked.retain(|(_, _, asset)| {
                if liq_fire::direct_dex_pool(asset, &usdc).is_some() { return true; }
                seen_asset.insert(*asset)
            });
            ranked.truncate(arm_max);
            let armed: HashSet<Pubkey> = ranked.iter().map(|(pk, _, _)| *pk).collect();
            cache.write().unwrap().retain(|pk, _| armed.contains(pk)); // evict no-longer-armed
            let bh = *blockhash.read().unwrap();
            let (mut no_cand, mut build_err, mut built_ok) = (0u32, 0u32, 0u32);
            let mut last_err = String::new();
            for (pk, _, _) in &ranked {
                let stale = cache.read().unwrap().get(pk).map(|c| c.built.elapsed() > arm_rebuild).unwrap_or(true);
                if !stale { continue; }
                let a = { let s = state.read().unwrap(); s.accounts.get(pk).cloned() };
                let Some(a) = a else { continue };
                let cand = { let s = state.read().unwrap();
                    let ob = s.obs_banks.get(pk).cloned().unwrap_or_default();
                    build_candidate(&a, pk, &s.banks, &s.oracle_of, &mint_tp, &ob) };
                let Some(cand) = cand else { no_cand += 1; continue };
                // Crankable collateral (Pyth sponsored) → fire as a Jito crank bundle
                // (tip a Jito account); else plain Sender (tip a Helius account).
                let is_crank = crank_on && crankable_c.contains(&cand.asset_bank);
                let tip = if is_crank { jito_tip } else { helius_tip };
                // Measurement mode: cache a placeholder (no Jupiter) so the hot path
                // has an armed set to time. NOT fireable — DRY-RUN measurement only.
                if synth {
                    built_ok += 1;
                    cache.write().unwrap().insert(*pk, CachedFire { tx: VersionedTransaction::default(), seize: cand.asset_amount, quoted_out: 0, built: Instant::now(), asset_bank: cand.asset_bank, crank: is_crank });
                    continue;
                }
                match liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority, tip,
                    (tip_sol * 1e9) as u64, 100_000, slippage_bps, 20, bh) {
                    Ok(f) => {
                        // Verify off the hot path. A crank candidate reads 6068 (healthy on-chain)
                        // in a plain sim — that's expected (it becomes liquidatable AFTER the crank),
                        // so sim_cacheable tolerates 6068. Structural failures are still rejected.
                        let (cacheable, why) = if verify_arm { sim_cacheable(&endpoint, &f.tx) } else { (true, "unverified".into()) };
                        if cacheable {
                            built_ok += 1;
                            cache.write().unwrap().insert(*pk, CachedFire { tx: f.tx, seize: cand.asset_amount, quoted_out: f.quoted_usdc_out, built: Instant::now(), asset_bank: cand.asset_bank, crank: is_crank });
                        } else { build_err += 1; last_err = format!("sim reject {}: {}", &pk.to_string()[..8], why); }
                    }
                    Err(e) => { build_err += 1; last_err = e.to_string(); }
                }
                // Space out Jupiter quotes to stay under the rate limit (429).
                std::thread::sleep(Duration::from_millis(quote_gap_ms));
            }
            if std::env::var("ARM_DEBUG").is_ok() {
                eprintln!("[arm] book {n_book} now-liq {n_now} triggers {n_trig} → armed {} cache {} | no_cand {no_cand} build_err {build_err} built_ok {built_ok}{}",
                    ranked.len(), cache.read().unwrap().len(),
                    if last_err.is_empty() { String::new() } else { format!(" | last_err: {}", &last_err[..last_err.len().min(90)]) });
            }
            // Triggers are stable price LEVELS (balances drift slowly), so a few-second
            // rebuild is plenty — the hot path catches crossings against the live price.
            let secs = std::env::var("TRIGGER_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(2);
            std::thread::sleep(Duration::from_secs(secs));
        });
    }

    eprintln!("[stream-exec] marginfi FAST executor {} authority={authority} full-book={n_book} arm_max={arm_max} arm_band={arm_band}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" });

    // ── HOT LOOP: Lazer tick → health over ARMED set (µs) → fire cached (≈1ms) ──
    let mut last_tick_us: u64 = 0;
    let mut handled: HashMap<Pubkey, Instant> = HashMap::new();
    let handle_cd = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(15));
    let mut decide_samples: Vec<f64> = Vec::new();
    let mut last_hb = Instant::now();
    loop {
        // Block until a fresh Lazer tick (in-memory poll).
        let deadline = Instant::now() + Duration::from_millis(500);
        loop {
            let cur = lazer::arm_feed_ids().into_iter().filter_map(|f| pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            if cur > last_tick_us { last_tick_us = cur; break; }
            if Instant::now() >= deadline { break; }
            std::thread::sleep(Duration::from_millis(1));
        }
        let t_tick = now_us();

        // FULL-BOOK detection: binary-search the per-bank trigger index against the
        // live blended price. O(log n) per moved asset — covers EVERY account, not
        // just a pre-armed set, so a crash from healthy is caught the tick it crosses.
        let slot = cur_slot.load(Ordering::Relaxed);
        let (crossed, n_trig): (Vec<Pubkey>, usize) = {
            let s = state.read().unwrap();
            let base = s.fresh_base(slot, default_stale);
            let (prices, _) = lazer::blend(&s.banks, &base, &lazer_table, &lazer_map);
            let trig = triggers.read().unwrap();
            let n_trig: usize = trig.values().map(|v| v.len()).sum();
            let mut out: Vec<Pubkey> = Vec::new();
            for (bank, list) in trig.iter() {
                let Some(&p) = prices.get(bank) else { continue };
                let k = list.partition_point(|x| x.0 >= p); // sorted DESC: all before k have trigger ≥ live price = crossed
                for (_, pk) in &list[..k] {
                    if handled.get(pk).is_some_and(|t| t.elapsed() < handle_cd) { continue; }
                    // Re-verify at current prices (guards a stale trigger between rebuilds).
                    if let Some(a) = s.accounts.get(pk) {
                        let h = liq::maintenance_health(a, &s.banks, &prices);
                        if h.missing == 0 && h.health.ratio() >= 1.0 + underwater_margin && h.health.weighted_assets >= min_collateral {
                            out.push(*pk);
                        }
                    }
                }
            }
            out.sort(); out.dedup();
            (out, n_trig)
        };
        // Hot-path decision latency: tick → full-book crossing verdict.
        let decide_us = (now_us() - t_tick) as f64;
        decide_samples.push(decide_us / 1000.0);
        if last_hb.elapsed() > Duration::from_secs(5) && !decide_samples.is_empty() {
            decide_samples.sort_by(|a, b| a.partial_cmp(b).unwrap());
            let med = decide_samples[decide_samples.len() / 2];
            let p90 = decide_samples[(decide_samples.len() * 9 / 10).min(decide_samples.len() - 1)];
            eprintln!("[hb] triggers {n_trig} cache {} | hot-path decide: median {:.3}ms p90 {:.3}ms (n={}) | crossed {}",
                cache.read().unwrap().len(), med, p90, decide_samples.len(), crossed.len());
            decide_samples.clear();
            last_hb = Instant::now();
        }

        for pk in crossed {
            handled.insert(pk, Instant::now());
            let fresh_bh = *blockhash.read().unwrap();
            // Grab the pre-built fire, or build it on-the-fly (the "tail": an account
            // that crashed faster than the arm-band could pre-build — rarer, few ms).
            let cached = { let c = cache.read().unwrap();
                c.get(&pk).map(|cf| (cf.tx.clone(), cf.seize, cf.quoted_out, cf.crank, cf.asset_bank, cf.built.elapsed())) };
            let (mut tx, seize, quoted_out, is_crank, asset_bank, built_ago) = match cached {
                Some(x) => x,
                None => {
                    let a = { let s = state.read().unwrap(); s.accounts.get(&pk).cloned() };
                    let Some(a) = a else { continue };
                    let cand = { let s = state.read().unwrap(); let ob = s.obs_banks.get(&pk).cloned().unwrap_or_default();
                        build_candidate(&a, &pk, &s.banks, &s.oracle_of, &mint_tp, &ob) };
                    let Some(cand) = cand else { continue };
                    let ic = crank_on && crankable.contains(&cand.asset_bank);
                    let tip = if ic { jito_tip } else { helius_tip };
                    match liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority, tip,
                        (tip_sol * 1e9) as u64, 100_000, slippage_bps, 20, fresh_bh) {
                        Ok(f) => (f.tx, cand.asset_amount, f.quoted_usdc_out, ic, cand.asset_bank, Duration::from_millis(0)),
                        Err(_) => continue,
                    }
                }
            };
            let decide_ms = (now_us() - t_tick) as f64 / 1000.0;
            if dry_run {
                log_line(&run_dir, &serde_json::json!({"t":now(),"liquidatee":pk.to_string(),"seize":seize,"mode":if is_crank {"crank"} else {"sender"},
                    "quoted_out":quoted_out,"decide_ms":decide_ms,"armed_ms_ago":built_ago.as_millis(),"dry_run":true}).to_string());
                continue;
            }
            let Some(kp) = kp.as_ref() else { continue };
            // Sign the liquidate tx (bundle tail, or the sole Sender tx).
            tx.message.set_recent_blockhash(fresh_bh);
            tx.signatures[0] = kp.sign_message(&tx.message.serialize());
            use base64::Engine;
            let liq_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
            let sig = tx.signatures[0].to_string();
            if sim_only {
                let (ok, err, units) = simulate_tx(&endpoint, &liq_b64);
                log_line(&run_dir, &serde_json::json!({"t":now(),"liquidatee":pk.to_string(),"seize":seize,"mode":if is_crank {"crank"} else {"sender"},
                    "quoted_out":quoted_out,"SIM_ONLY":true,"sim_ok":ok,"units":units,"sim_err":err}).to_string());
                continue;
            }
            if is_crank {
                // CRANK BUNDLE: [crank_setup, crank_fire(posts fresh Pyth price), liquidate].
                // Atomic — the bundle only lands if the liquidate succeeds at the freshly
                // posted price, so it's self-gating (no tip paid on a miss).
                let Some(feed_id) = feed_of.get(&asset_bank).copied() else {
                    log_line(&run_dir, &format!("crank skip {}: no feed", &pk.to_string()[..8])); continue };
                let (mu, vaa, age) = match hermes.update_for(&feed_id) {
                    Some(x) => x, None => { log_line(&run_dir, &format!("crank skip {}: no Hermes blob", &pk.to_string()[..8])); continue } };
                if age > max_blob_age { log_line(&run_dir, &format!("crank skip {}: blob stale {age:?}", &pk.to_string()[..8])); continue; }
                let mut ctxs = match pyth_crank::build_crank_txs(&authority, &vaa, std::slice::from_ref(&mu), 0, 0, fresh_bh) {
                    Ok(c) => c, Err(e) => { log_line(&run_dir, &format!("crank build fail {}: {e}", &pk.to_string()[..8])); continue } };
                ctxs.stamp_and_sign(kp, fresh_bh);
                let (setup_b64, crank_b64) = match ctxs.to_b64() { Ok(x) => x, Err(e) => { log_line(&run_dir, &format!("crank b64 fail: {e}")); continue } };
                let res = jito::send_bundle(&block_engine, &[setup_b64, crank_b64, liq_b64]);
                let submit_ms = (now_us() - t_tick) as f64 / 1000.0;
                log_line(&run_dir, &serde_json::json!({"t":now(),"liquidatee":pk.to_string(),"seize":seize,"mode":"crank",
                    "decide_ms":decide_ms,"submit_ms":submit_ms,"signature":sig,"bundle":res.as_ref().ok(),
                    "sent":res.is_ok(),"send_err":res.as_ref().err().map(|e| e.to_string()),"blob_age_ms":age.as_millis(),"fired":true}).to_string());
            } else {
                let res = jito::send_sender(&sender_url, &liq_b64);
                let submit_ms = (now_us() - t_tick) as f64 / 1000.0;
                log_line(&run_dir, &serde_json::json!({"t":now(),"liquidatee":pk.to_string(),"seize":seize,"mode":"sender",
                    "decide_ms":decide_ms,"submit_ms":submit_ms,"signature":sig,"sent":res.is_ok(),"send_err":res.as_ref().err().map(|e| e.to_string()),"fired":true}).to_string());
            }
        }
    }
}

/// Build a FireCandidate from LIVE state (no fetch): largest collateral × a
/// wired-debt leg, full observation list.
fn build_candidate(a: &MarginfiAccount, pk: &Pubkey, banks: &BankMap,
    oracle_of: &HashMap<Pubkey, Pubkey>, mint_tp: &HashMap<Pubkey, Pubkey>, obs_banks: &[Pubkey]) -> Option<FireCandidate> {
    let asset = a.balances.iter().filter(|b| b.asset_shares > 0.0)
        .max_by(|x, y| x.asset_shares.partial_cmp(&y.asset_shares).unwrap_or(std::cmp::Ordering::Equal))?;
    let debt = a.balances.iter().filter(|b| b.liability_shares > 0.0)
        .find(|b| banks.get(&b.bank_pk).map(|bk| is_debt_mint(&bk.mint)).unwrap_or(false))?;
    let abk = banks.get(&asset.bank_pk)?;
    let lbk = banks.get(&debt.bank_pk)?;
    let native = asset.asset_shares * abk.asset_share_value;
    let seize = (native * 0.5) as u64;
    if seize == 0 { return None; }
    let asset_tp = *mint_tp.get(&abk.mint)?;
    let debt_tp = *mint_tp.get(&lbk.mint)?;
    // Observation list covers ALL active-flag banks (incl. zero-share) — marginfi
    // requires an oracle for each or it fails 6051. Falls back to the funded
    // balances if the active-bank list is somehow empty.
    let mut obs = Vec::new();
    let bank_list: Vec<Pubkey> = if obs_banks.is_empty() { a.balances.iter().map(|b| b.bank_pk).collect() } else { obs_banks.to_vec() };
    for bank_pk in &bank_list {
        let oc = oracle_of.get(bank_pk)?;
        obs.push(AccountMeta::new_readonly(*bank_pk, false));
        obs.push(AccountMeta::new_readonly(*oc, false));
    }
    Some(FireCandidate {
        liquidatee: *pk, asset_bank: asset.bank_pk, asset_mint: abk.mint, asset_token_program: asset_tp,
        asset_amount: seize, liab_bank: debt.bank_pk, debt_mint: lbk.mint, debt_token_program: debt_tp,
        asset_oracle: *oracle_of.get(&asset.bank_pk)?, liab_oracle: *oracle_of.get(&debt.bank_pk)?,
        liquidatee_obs: obs,
    })
}

/// One gRPC subscription lifecycle: decode each account update into the live maps.
/// Oracle accounts are stored RAW (fresh-decoded at use); MA/Bank are decoded.
async fn run_stream(endpoint: &str, x_token: &str, sub: &[String], oracle_set: &HashSet<Pubkey>,
    state: &Arc<RwLock<LiveState>>) -> Result<()> {
    let mut client = GeyserGrpcClient::build_from_shared(endpoint.to_string())?
        .x_token(Some(x_token.to_string()))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect().await?;
    let mut accs = HashMap::new();
    accs.insert("liq".to_string(), SubscribeRequestFilterAccounts {
        account: sub.to_vec(), owner: vec![], filters: vec![], ..Default::default() });
    let req = SubscribeRequest { accounts: accs, commitment: Some(CommitmentLevel::Processed as i32), ..Default::default() };
    let (mut _sink, mut stream) = client.subscribe_with_request(Some(req)).await?;
    while let Some(msg) = stream.next().await {
        if let Some(UpdateOneof::Account(acc)) = msg?.update_oneof {
            if let Some(info) = acc.account {
                let Ok(pk) = Pubkey::try_from(info.pubkey.as_slice()) else { continue };
                if oracle_set.contains(&pk) {
                    state.write().unwrap().oracle_raw.insert(pk, info.data);
                } else if info.data.len() == liq::MA_SIZE {
                    if let Some(a) = MarginfiAccount::decode(&info.data) {
                        let obs = liq::active_bank_pks(&info.data);
                        let mut w = state.write().unwrap();
                        w.accounts.insert(pk, a);
                        w.obs_banks.insert(pk, obs);
                    }
                } else if let Some(b) = Bank::decode(&info.data) {
                    state.write().unwrap().banks.insert(pk, b);
                }
            }
        }
    }
    Ok(())
}

fn log_line(run_dir: &str, s: &str) {
    use std::io::Write;
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(format!("{run_dir}/stream.jsonl")) {
        let _ = writeln!(f, "{s}");
    }
    eprintln!("[fire] {s}");
}
