//! Production marginfi liquidation executor — continuous loop, DRY_RUN default.
//!
//! Detection is simulation-gated (the emode lesson: don't replicate marginfi's
//! risk math off-chain — let the chain judge). Pipeline per candidate:
//!
//!   full scan (RESCAN_SECS) → watch-set of near-liquidation borrowers
//!   fast poll (POLL_MS): fresh watch-set accounts + bank/oracle prices
//!   base-weight liquidatable? → sim-gate [start_fl, liquidate, end_fl]
//!   → SIZE the seize by simulation ladder (largest passing fraction)
//!   → build the atomic fire tx (liquidate→withdraw→Jupiter swap→repay_all)
//!   → profit gate (quoted USDC out vs ~97.5% liability taken + tip)
//!   → FULL fire-tx simulation (ground truth for every leg incl. swap+repay)
//!   → DRY_RUN: log · LIVE: sign + submit via Helius Sender, readback P&L
//!
//! ── Self-crank mode (the stale-oracle edge) ─────────────────────────────
//! marginfi's Pyth feeds lag the true price by 8–44s. When an account is
//! underwater at the TRUE (Lazer-blended) price but still healthy at on-chain
//! prices, the Sender path can't fire — the chain would judge it healthy. If
//! the asset bank's oracle is a shard-0 sponsored feed (permissionless crank),
//! we instead fire an atomic Jito bundle:
//!
//!   [crank_setup, crank_fire (posts the fresh Hermes price), liquidate]
//!
//! Sizing + ground truth for these run through simulateBundle so the chain
//! judges AT the cranked price. The Hermes blob is kept hot by a background
//! poll; crank txs are rebuilt from the freshest blob at fire time. The bundle
//! is all-or-nothing: a losing fire never lands, pays nothing.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [PYTH_LAZER_TOKEN=… (required for the crank edge)] [CRANK=1]
//!        [MIN_COLLATERAL_USD=100] [MIN_PROFIT_USD=0.5] [TIP_FRACTION_BPS=3000]
//!        [POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.85] [RUN_DIR=runs]
//!        [MAX_BLOB_AGE_MS=3000] [JITO_BLOCK_ENGINE=…]
//!        cargo run --release --bin liq_executor

use arb_engine::jito::{bundle_status, default_block_engine, get_tip_accounts, send_bundle, send_sender};
use arb_engine::liq_fire::{self, FireCandidate};
use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use arb_engine::pyth_accumulator::{spawn_hermes_cache, HermesCache};
use arb_engine::pyth_crank;
use serde::Serialize;
use solana_instruction::AccountMeta;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const SOL_MINT: &str = "So11111111111111111111111111111111111111112";
const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";

/// Debt (liability) assets the fire path can repay: USDC, USDT, wSOL. The
/// liquidator absorbs the liquidatee's liability and repays it by swapping the
/// seized collateral into this asset — so it must be a mint Jupiter routes
/// liquidly and the marginfi flashloan can repay.
fn is_debt_mint(mint: &Pubkey) -> bool {
    let m = mint.to_string();
    m == marginfi::USDC_MINT || m == USDT_MINT || m == SOL_MINT
}
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const HEALTHY_ACCOUNT_ERR: u32 = 6068;
/// Largest→smallest: bigger seize = more profit; marginfi rejects over-
/// liquidation (post-liq health must stay ≤ 0), so walk down until one passes.
const SIZE_LADDER: [f64; 5] = [1.0, 0.5, 0.25, 0.1, 0.02];

fn now() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }
fn now_us() -> u128 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_micros() }

/// Latency ledger: proves whether SPEED is the bottleneck. `appeared_us` is the
/// Lazer PUBLISH timestamp of the price that made the account cross (the moment
/// the opportunity truly exists); the deltas measure how long WE take from that
/// instant to detect and to submit. Appended to {run_dir}/latency.jsonl.
fn log_latency(run_dir: &str, v: &serde_json::Value) {
    let _ = std::fs::create_dir_all(run_dir);
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(format!("{run_dir}/latency.jsonl")) {
        use std::io::Write;
        let _ = writeln!(f, "{v}");
    }
}

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
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
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"base64"}]}))?;
    v["result"]["value"]["owner"].as_str()?.parse().ok()
}
fn latest_blockhash(endpoint: &str) -> Option<solana_hash::Hash> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))?;
    solana_hash::Hash::from_str(v["result"]["value"]["blockhash"].as_str()?).ok()
}
fn sol_balance(endpoint: &str, owner: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[owner]}))
        .and_then(|v| v["result"]["value"].as_u64()).map(|l| l as f64 / 1e9).unwrap_or(0.0)
}
fn current_slot(endpoint: &str) -> u64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSlot","params":[{"commitment":"confirmed"}]}))
        .and_then(|v| v["result"].as_u64()).unwrap_or(0)
}

fn simulate_tx_b64(endpoint: &str, b64tx: &str) -> Option<serde_json::Value> {
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))?;
    sim.get("result")?.get("value").cloned()
}

/// The sizing-gate tx [start_fl, liquidate(asset_amount), end_fl] as base64 —
/// simulated standalone (Sender path) or as the tail of a crank bundle.
#[allow(clippy::too_many_arguments)]
fn gate_tx_b64(
    authority: &Pubkey, liquidator_ma: &Pubkey, tp: &Pubkey,
    liquidatee: &Pubkey, acct: &MarginfiAccount, asset_bank: Pubkey, liab_bank: Pubkey,
    asset_amount: u64, oracle_of: &HashMap<Pubkey, Pubkey>,
) -> Option<String> {
    use solana_message::{v0, VersionedMessage};
    let mut liquidatee_obs = Vec::new();
    for b in &acct.balances {
        liquidatee_obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        liquidatee_obs.push(AccountMeta::new_readonly(*oracle_of.get(&b.bank_pk)?, false));
    }
    let start = marginfi::start_flashloan(liquidator_ma, authority, 2);
    let liq_ix = marginfi::lending_account_liquidate(
        &asset_bank, &liab_bank, liquidator_ma, authority, liquidatee, tp, asset_amount,
        oracle_of.get(&asset_bank)?, oracle_of.get(&liab_bank)?, &liquidatee_obs);
    let end_obs = vec![
        AccountMeta::new_readonly(asset_bank, false), AccountMeta::new_readonly(*oracle_of.get(&asset_bank)?, false),
        AccountMeta::new_readonly(liab_bank, false), AccountMeta::new_readonly(*oracle_of.get(&liab_bank)?, false),
    ];
    let end = marginfi::end_flashloan(liquidator_ma, authority, &end_obs);
    let msg = v0::Message::try_compile(authority, &[start, liq_ix, end], &[], solana_hash::Hash::default()).ok()?;
    let tx = VersionedTransaction { signatures: vec![Default::default()], message: VersionedMessage::V0(msg) };
    use base64::Engine;
    Some(base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).ok()?))
}

/// Cheap sim gate: Some(true) = marginfi accepts the liquidation at this size.
/// With crank txs, the gate rides behind them in a simulateBundle so the chain
/// judges at the CRANKED price; standalone it judges at on-chain prices.
#[allow(clippy::too_many_arguments)]
fn simulate_gate(
    endpoint: &str, authority: &Pubkey, liquidator_ma: &Pubkey, tp: &Pubkey,
    liquidatee: &Pubkey, acct: &MarginfiAccount, asset_bank: Pubkey, liab_bank: Pubkey,
    asset_amount: u64, oracle_of: &HashMap<Pubkey, Pubkey>,
    crank_b64: Option<&(String, String)>,
) -> Option<bool> {
    let gate = gate_tx_b64(authority, liquidator_ma, tp, liquidatee, acct,
        asset_bank, liab_bank, asset_amount, oracle_of)?;
    match crank_b64 {
        Some((setup, fire)) => {
            let sim = simulate_bundle(endpoint, &[setup.clone(), fire.clone(), gate])?;
            if sim.ran_ok == 3 { return Some(true); }
            // Crank txs must not be the failure — that's a broken crank, not a
            // healthy account; surface as None so the caller doesn't cool down.
            if sim.ran_ok < 2 { return None; }
            Some(false)
        }
        None => {
            let res = simulate_tx_b64(endpoint, &gate)?;
            let err = &res["err"];
            if err.is_null() { return Some(true); }
            let code = err.get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
            match code {
                Some(c) if c as u32 == HEALTHY_ACCOUNT_ERR => Some(false),
                Some(_) => Some(false), // wrong size / other guard — try another rung
                None => None,
            }
        }
    }
}

#[derive(Serialize)]
struct DecisionLog {
    t: u64, liquidatee: String, mode: String, collateral_usd: f64, ratio: f64,
    seize_native: u64, quoted_usdc_out: f64, est_liab_usdc: f64, est_profit_usdc: f64,
    fire_sim_ok: bool, fired: bool, reason: String,
}
#[derive(Serialize)]
struct TradeLog {
    t: u64, liquidatee: String, seize_native: u64, est_profit_usdc: f64,
    tip_lamports: u64, signature: Option<String>, bundle: Option<String>,
    realized_usdc: Option<f64>, error: Option<String>,
}

/// How a cached fire gets submitted.
#[derive(Clone)]
enum FireMode {
    /// Single tx via Helius Sender — the account is already underwater at
    /// on-chain prices, no crank needed.
    Sender,
    /// Jito bundle [crank_setup, crank_fire, liquidate] — underwater at the
    /// true price only; the crank makes the chain agree. Crank txs are built
    /// at fire time from the freshest Hermes blob for this feed.
    Crank { feed_id: [u8; 32] },
}

impl FireMode {
    fn name(&self) -> &'static str {
        match self { FireMode::Sender => "sender", FireMode::Crank { .. } => "crank" }
    }
}

/// The shape the fire path can act on (matches `try_arm`): exactly one
/// collateral and exactly one liability whose bank is a supported debt asset
/// (USDC/USDT/wSOL). Anything else (multi-position, exotic debt) is silently
/// skipped downstream, so the watch-set/engine must not track or rank it — else
/// its "liquidatable" count is dominated by un-fireable accounts and
/// deficit-ranking starves real ones.
fn is_v1_fireable(a: &MarginfiAccount, banks: &BankMap) -> bool {
    let assets = a.balances.iter().filter(|b| b.asset_shares > 0.0).count();
    let liabs: Vec<&Pubkey> = a.balances.iter().filter(|b| b.liability_shares > 0.0).map(|b| &b.bank_pk).collect();
    assets == 1 && liabs.len() == 1 && banks.get(liabs[0]).map(|b| is_debt_mint(&b.mint)).unwrap_or(false)
}

/// Everything the crank path needs, spun up once at boot.
struct CrankCtx {
    on: bool,
    hermes: HermesCache,
    tips: Vec<Pubkey>,
    block_engine: String,
    max_blob_age: Duration,
}

impl CrankCtx {
    fn pick_tip(&self) -> Option<Pubkey> {
        if self.tips.is_empty() { return None; }
        Some(self.tips[now() as usize % self.tips.len()])
    }
}

struct Scan {
    accts: Vec<(Pubkey, MarginfiAccount)>,
    banks: BankMap,
    oracle_of: HashMap<Pubkey, Pubkey>,
    /// bank → 32-byte Pyth feed id, decoded from the oracle account itself.
    feed_of: HashMap<Pubkey, [u8; 32]>,
    /// Banks whose oracle IS the shard-0 sponsored feed PDA — the ones we can
    /// permissionlessly crank (write_authority == the feed itself).
    crankable: HashSet<Pubkey>,
}

fn full_scan(endpoint: &str) -> Option<Scan> {
    let resp = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]}))?;
    let entries = resp["result"].as_array()?.clone();
    let accts: Vec<(Pubkey, MarginfiAccount)> = entries.iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0)).collect();
    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk)).collect::<HashSet<_>>().into_iter().collect();
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of = HashMap::new();
    for (pk, raw) in &get_multiple(endpoint, &bank_pks) {
        if let Some(bk) = Bank::decode(raw) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); }
    }
    // Crank metadata: decode each oracle's feed id and check whether the
    // oracle is the shard-0 sponsored PDA for that feed (→ crankable).
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(endpoint, &oracle_pks);
    let mut feed_of = HashMap::new();
    let mut crankable = HashSet::new();
    for (bank, oracle) in &oracle_of {
        let Some((fid, _, _)) = oracle_raw.get(oracle).and_then(|r| liq::decode_price_update_v2(r)) else { continue };
        feed_of.insert(*bank, fid);
        if pyth_crank::sponsored_feed(0, &fid) == *oracle { crankable.insert(*bank); }
    }
    Some(Scan { accts, banks, oracle_of, feed_of, crankable })
}

// ── simulateBundle plumbing (crank mode judges through the fresh price) ─────

/// How many leading txs in the bundle succeeded. jito-solana stops at the first
/// failing tx, so `ran_ok < n` means tx[ran_ok] reverted. For the crank bundle
/// [setup, fire, gate], `ran_ok == 3` = accepted; `2` = crank landed but the
/// liquidate reverted; `< 2` = the crank itself failed.
struct BundleSim {
    ran_ok: usize,
}

fn simulate_bundle(endpoint: &str, txs_b64: &[String]) -> Option<BundleSim> {
    let nulls = vec![serde_json::Value::Null; txs_b64.len()];
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateBundle",
        "params":[{"encodedTransactions": txs_b64}, {
            "skipSigVerify": true, "replaceRecentBlockhash": true,
            "preExecutionAccountsConfigs": nulls, "postExecutionAccountsConfigs": nulls,
        }]}))?;
    if v.get("error").filter(|e| !e.is_null()).is_some() { return None; }
    let results = v["result"]["value"]["transactionResults"].as_array().cloned().unwrap_or_default();
    let ran_ok = results.iter().take_while(|r| r["err"].is_null()).count();
    Some(BundleSim { ran_ok })
}

fn fresh_prices(endpoint: &str, oracle_of: &HashMap<Pubkey, Pubkey>) -> PriceMap {
    // A stale Switchboard oracle is dropped here (see decode_oracle_price_fresh):
    // the account then reads as `missing` and is never trusted as liquidatable,
    // matching the chain's SwitchboardStalePrice(6049) gate. One getSlot per
    // rescan (off the tick path).
    let slot = current_slot(endpoint);
    let max_stale: u64 = std::env::var("MAX_SB_STALE_SLOTS").ok().and_then(|s| s.parse().ok())
        .unwrap_or(liq::DEFAULT_MAX_SB_STALE_SLOTS);
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let mut by_oracle: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, raw) in &get_multiple(endpoint, &oracle_pks) {
        if let Some(usd) = liq::decode_oracle_price_fresh(raw, slot, max_stale) { by_oracle.insert(*pk, usd); }
    }
    oracle_of.iter().filter_map(|(bk, oc)| Some((*bk, *by_oracle.get(oc)?))).collect()
}

/// Copy-able config bundle for the arm/fire helpers.
#[derive(Clone, Copy)]
struct Cfg {
    liquidator_ma: Pubkey,
    authority: Pubkey,
    tp: Pubkey,
    /// Kept for boot-time config/logging; the fire path now resolves the actual
    /// debt bank per account (USDC/USDT/wSOL) rather than assuming this one.
    #[allow(dead_code)]
    usdc_bank: Pubkey,
    tip_account: Pubkey,
    tip_fraction_bps: u64,
    min_tip_sol: f64,
    min_profit: f64,
    slippage_bps: u32,
}

/// A fully-built, sim-verified fire tx kept hot for an armed account. The tx is
/// compiled with a placeholder blockhash (sim uses replaceRecentBlockhash); a
/// real blockhash is stamped at fire time. Sending it needs only sign+submit —
/// no quote, no sim, no RPC on the critical path.
#[derive(Clone)]
struct CachedFire {
    tx: VersionedTransaction,
    mode: FireMode,
    tip_lamports: u64,
    tip_sol: f64,
    est_profit: f64,
    seize: u64,
    built: Instant,
}

/// Build + size + profit-gate + full-sim-gate one account into a CachedFire.
/// Returns None if the account isn't a v1-shaped candidate, sizing fails (emode
/// phantom), the profit gate fails, or the fire-tx simulation reverts. Writes a
/// DecisionLog for the informative skips. This is the ONLY place a fire tx is
/// built — the arm phase caches it ahead of the cross; the sim lives here, off
/// the fire critical path.
#[allow(clippy::too_many_arguments)]
fn try_arm(
    endpoint: &str, run_dir: &str, cfg: &Cfg, crank: &CrankCtx, scan: &Scan,
    a: &MarginfiAccount, pk: &Pubkey, prices: &PriceMap, base: &PriceMap,
    mint_tp: &mut HashMap<Pubkey, Pubkey>,
) -> Option<CachedFire> {
    let r = liq::maintenance_health(a, &scan.banks, prices);
    let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).cloned().collect();
    let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).cloned().collect();
    if assets.len() != 1 || liabs.len() != 1 { return None; }
    // v1.5: the absorbed liability may be any of USDC/USDT/wSOL (the swap targets
    // it and payback_asset closes it). Reject anything else — no liquid route.
    let liab_bank = liabs[0].bank_pk;
    let liab_bank_info = scan.banks.get(&liab_bank)?;
    if !is_debt_mint(&liab_bank_info.mint) { return None; }
    let asset_bank = assets[0].bank_pk;
    let bank = scan.banks.get(&asset_bank)?;
    let native_total = assets[0].asset_shares * bank.asset_share_value;

    // Record why a flagged account did NOT fire (so the steady state is
    // observable — otherwise these rejects are silent). Gated by the caller's
    // handle/sim cooldowns, so it's ~a row per account per cooldown, not spam.
    let log_skip = |mode: &str, reason: &str| {
        log_decision(run_dir, &DecisionLog {
            t: now(), liquidatee: pk.to_string(), mode: mode.into(),
            collateral_usd: r.health.weighted_assets, ratio: r.health.ratio(),
            seize_native: 0, quoted_usdc_out: 0.0, est_liab_usdc: 0.0, est_profit_usdc: 0.0,
            fire_sim_ok: false, fired: false, reason: reason.into(),
        });
    };

    // Mode: already underwater at ON-CHAIN prices → plain Sender tx. Healthy
    // on-chain but underwater at the true (blended) price → the stale-window
    // edge: crank + liquidate as one bundle. Requires a crankable oracle and a
    // Hermes blob covering its feed.
    let onchain = liq::maintenance_health(a, &scan.banks, base);
    let mode = if onchain.missing == 0 && onchain.health.liquidatable() {
        FireMode::Sender
    } else {
        if !crank.on { return None; }
        // Below the true-price threshold the chain refuses even WITH a fresh
        // crank — don't burn bundle sims; the fire phase re-arms on the cross.
        if r.missing > 0 || !r.health.liquidatable() { return None; }
        // Crankable check FIRST: it covers non-Pyth (Switchboard) collateral,
        // whose feed_of lookup would otherwise silently short-circuit. Crankable
        // ⇒ shard-0 sponsored Pyth ⇒ feed_of is present.
        if !scan.crankable.contains(&asset_bank) {
            log_skip("crank", "flagged at Lazer price but healthy on-chain and oracle not crankable (non-Pyth/non-sponsored) — cannot act");
            return None;
        }
        let Some(feed_id) = scan.feed_of.get(&asset_bank).copied() else {
            log_skip("crank", "crankable but feed id missing — cannot build crank");
            return None;
        };
        match crank.hermes.update_for(&feed_id) {
            Some(_) => {}
            None => { log_skip("crank", "crankable but no fresh Hermes blob for feed yet"); return None; }
        }
        FireMode::Crank { feed_id }
    };

    // Crank txs for the sizing/ground-truth bundles (placeholder blockhash —
    // sims replace it; the LIVE fire rebuilds from the freshest blob anyway).
    let crank_b64: Option<(String, String)> = match &mode {
        FireMode::Sender => None,
        FireMode::Crank { feed_id } => {
            let (mu, vaa, _age) = crank.hermes.update_for(feed_id)?;
            let txs = pyth_crank::build_crank_txs(&cfg.authority, &vaa, std::slice::from_ref(&mu),
                0, 0, solana_hash::Hash::default()).ok()?;
            Some(txs.to_b64().ok()?)
        }
    };

    // Size by simulation ladder, largest passing fraction first.
    let mut seize = 0u64;
    for frac in SIZE_LADDER {
        let amount = (native_total * frac) as u64;
        if amount == 0 { continue; }
        if simulate_gate(endpoint, &cfg.authority, &cfg.liquidator_ma, &cfg.tp, pk, a, asset_bank,
            liab_bank, amount, &scan.oracle_of, crank_b64.as_ref()) == Some(true) {
            seize = amount;
            break;
        }
    }
    if seize == 0 {
        // The chain judged the account healthy at the price we can act on — for
        // crank mode that means Lazer flagged it but the Hermes-cranked price
        // isn't low enough for marginfi to agree (Lazer leads Hermes). Expected
        // over-flag; log it so it's visible, then the caller cools it down.
        log_skip(mode.name(), "chain says healthy at the actionable price (Lazer over-flag / not truly liquidatable)");
        return None;
    }

    let asset_tp = match mint_tp.get(&bank.mint) {
        Some(t) => *t,
        None => { let t = mint_owner(endpoint, &bank.mint)?; mint_tp.insert(bank.mint, t); t }
    };
    let debt_mint = liab_bank_info.mint;
    let debt_tp = match mint_tp.get(&debt_mint) {
        Some(t) => *t,
        None => { let t = mint_owner(endpoint, &debt_mint)?; mint_tp.insert(debt_mint, t); t }
    };
    let mut obs = Vec::new();
    for b in &a.balances {
        let oc = scan.oracle_of.get(&b.bank_pk)?;
        obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        obs.push(AccountMeta::new_readonly(*oc, false));
    }
    let cand = FireCandidate {
        liquidatee: *pk, asset_bank, asset_mint: bank.mint, asset_token_program: asset_tp,
        asset_amount: seize, liab_bank,
        debt_mint, debt_token_program: debt_tp,
        asset_oracle: scan.oracle_of[&asset_bank], liab_oracle: scan.oracle_of[&liab_bank],
        liquidatee_obs: obs,
    };
    let price = prices.get(&asset_bank).copied().unwrap_or(0.0);
    let seized_usd = seize as f64 / 10f64.powi(bank.mint_decimals as i32) * price;
    let est_liab = seized_usd * 0.975;
    // Debt asset USD conversion: the swap output is native debt units, so a
    // non-USDC debt (USDT ≈ $1, wSOL ≈ $150) must be priced to compare against
    // the (USD) liability estimate. Fall back to $1 for a stablecoin debt bank
    // with no live price rather than mis-valuing it as free.
    let debt_dec = liab_bank_info.mint_decimals as i32;
    let debt_price = prices.get(&liab_bank).copied()
        .unwrap_or(if debt_mint.to_string() == SOL_MINT { 150.0 } else { 1.0 });
    let debt_out_usd = |native: u64| native as f64 / 10f64.powi(debt_dec) * debt_price;
    let sol_usd = scan.banks.iter().find(|(_, b)| b.mint.to_string() == SOL_MINT)
        .and_then(|(bk, _)| prices.get(bk)).copied().unwrap_or(150.0);

    let mut log = DecisionLog {
        t: now(), liquidatee: pk.to_string(), mode: mode.name().into(),
        collateral_usd: r.health.weighted_assets, ratio: r.health.ratio(),
        seize_native: seize, quoted_usdc_out: 0.0, est_liab_usdc: est_liab, est_profit_usdc: 0.0,
        fire_sim_ok: false, fired: false, reason: String::new(),
    };
    // Sender tips a Helius Sender wallet; a bundle must tip a Jito account.
    let tip_to = match &mode {
        FireMode::Sender => cfg.tip_account,
        FireMode::Crank { .. } => match crank.pick_tip() {
            Some(t) => t,
            None => { log.reason = "no Jito tip accounts".into(); log_decision(run_dir, &log); return None; }
        }
    };
    // Build with a placeholder blockhash (sim replaces it; fire stamps a real one).
    let ph = solana_hash::Hash::default();
    let fire = match liq_fire::build_fire_tx(endpoint, &cand, &cfg.liquidator_ma, &cfg.authority,
        Some(tip_to), 0, 100_000, cfg.slippage_bps, 20, ph) {
        Ok(f) => f,
        Err(e) => { log.reason = format!("build: {e}"); log_decision(run_dir, &log); return None; }
    };
    log.quoted_usdc_out = debt_out_usd(fire.quoted_usdc_out);
    let est_profit = debt_out_usd(fire.quoted_usdc_out) - est_liab;
    log.est_profit_usdc = est_profit;
    let tip_sol = (est_profit * cfg.tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(cfg.min_tip_sol);
    let tip_lamports = (tip_sol * 1e9) as u64;
    if est_profit < cfg.min_profit + tip_sol * sol_usd {
        log.reason = format!("below min profit (est ${est_profit:.2}, tip ${:.2})", tip_sol * sol_usd);
        log_decision(run_dir, &log);
        return None;
    }
    let fire = match liq_fire::build_fire_tx(endpoint, &cand, &cfg.liquidator_ma, &cfg.authority,
        Some(tip_to), tip_lamports, 100_000, cfg.slippage_bps, 20, ph) {
        Ok(f) => f,
        Err(e) => { log.reason = format!("rebuild: {e}"); log_decision(run_dir, &log); return None; }
    };
    // Ground-truth gate lives HERE (arm time), off the fire critical path. In
    // crank mode the whole bundle is the ground truth — the liquidate must
    // succeed AT the cranked price.
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
    let sim_ok = match &crank_b64 {
        None => simulate_tx_b64(endpoint, &b64tx).map(|res| res["err"].is_null()).unwrap_or(false),
        Some((s, c)) => simulate_bundle(endpoint, &[s.clone(), c.clone(), b64tx])
            .map(|r| r.ran_ok == 3).unwrap_or(false),
    };
    log.fire_sim_ok = sim_ok;
    if !sim_ok {
        log.reason = "fire sim revert (swap/repay would not cover liability)".into();
        log_decision(run_dir, &log);
        return None;
    }
    Some(CachedFire { tx: fire.tx, mode, tip_lamports, tip_sol, est_profit, seize, built: Instant::now() })
}

/// Fire a cached tx: stamp the fresh blockhash, sign, submit (Sender for a
/// plain fire; a Jito bundle with freshly-built crank txs for crank mode),
/// log, spawn the realized-P&L readback. The profit-or-revert guard makes this
/// safe without re-simulating — a stale/unprofitable Sender fire reverts for
/// the base fee, and a failing bundle never lands at all.
#[allow(clippy::too_many_arguments)]
fn fire_cached(
    endpoint: &str, run_dir: &str, sender_url: &str, cfg: &Cfg, crank: &CrankCtx, dry_run: bool,
    pk: &Pubkey, cached: &CachedFire, fresh_bh: solana_hash::Hash, kp: Option<&Keypair>,
    daily_tip: &std::sync::Arc<std::sync::Mutex<f64>>, max_daily_tip: f64, wallet_min: f64,
    webhook: &Option<String>,
) {
    let mode = cached.mode.name();
    let mut log = DecisionLog {
        t: now(), liquidatee: pk.to_string(), mode: mode.into(), collateral_usd: 0.0, ratio: 0.0,
        seize_native: cached.seize, quoted_usdc_out: 0.0, est_liab_usdc: 0.0, est_profit_usdc: cached.est_profit,
        fire_sim_ok: true, fired: false, reason: String::new(),
    };
    println!("★ LIQUIDATABLE [{mode}]  {}  seize {}  est profit ${:.2}  tip {:.5} SOL  (armed {:?} ago)",
        &pk.to_string()[..8], cached.seize, cached.est_profit, cached.tip_sol, cached.built.elapsed());
    if dry_run {
        log.reason = format!("dry-run: would fire ({mode}, armed)");
        log_decision(run_dir, &log);
        alert(webhook, "liq-dry", &format!("DRY-RUN {mode} liquidation: {} est profit ${:.2}", pk, cached.est_profit));
        return;
    }
    if *daily_tip.lock().unwrap() + cached.tip_sol > max_daily_tip {
        log.reason = "daily tip cap".into(); log_decision(run_dir, &log);
        alert(webhook, "liq-cap", "daily tip cap reached"); return;
    }
    if sol_balance(endpoint, &cfg.authority.to_string()) < wallet_min {
        log.reason = "wallet below floor".into(); log_decision(run_dir, &log);
        alert(webhook, "liq-floor", "wallet below floor — not firing"); return;
    }
    let mut tx = cached.tx.clone();
    tx.message.set_recent_blockhash(fresh_bh);
    let kp = kp.unwrap();
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    let tx_b64 = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap()) };
    let (seize, est_profit, tip_lamports, tip_sol) = (cached.seize, cached.est_profit, cached.tip_lamports, cached.tip_sol);

    // Submit: Sender for a plain fire, Jito bundle for crank mode.
    let submit: Result<Option<String>, String> = match &cached.mode {
        FireMode::Sender => send_sender(sender_url, &tx_b64).map(|_| None).map_err(|e| e.to_string()),
        FireMode::Crank { feed_id } => (|| {
            // Freshest blob → crank txs; the whole point is the newest price.
            let (mu, vaa, age) = crank.hermes.update_for(feed_id)
                .ok_or_else(|| "no Hermes blob for feed".to_string())?;
            if age > crank.max_blob_age {
                return Err(format!("Hermes blob stale ({age:?}) — not bundling"));
            }
            let mut ctxs = pyth_crank::build_crank_txs(&cfg.authority, &vaa, std::slice::from_ref(&mu),
                0, 0, fresh_bh).map_err(|e| e.to_string())?;
            ctxs.stamp_and_sign(kp, fresh_bh);
            let (setup_b64, crank_b64) = ctxs.to_b64().map_err(|e| e.to_string())?;
            let mut last = String::new();
            for attempt in 0..3 {
                match send_bundle(&crank.block_engine, &[setup_b64.clone(), crank_b64.clone(), tx_b64.clone()]) {
                    Ok(id) => return Ok(Some(id)),
                    Err(e) if e.to_string().contains("429") && attempt < 2 => {
                        last = e.to_string();
                        std::thread::sleep(Duration::from_millis(250));
                    }
                    Err(e) => return Err(e.to_string()),
                }
            }
            Err(last)
        })(),
    };

    log.fired = submit.is_ok(); log.reason = format!("fired ({mode}, armed cache)");
    log_decision(run_dir, &log);
    match submit {
        Ok(bundle_id) => {
            eprintln!("[exec] FIRED [{mode}] {sig}{}", bundle_id.as_deref().map(|b| format!(" bundle {b}")).unwrap_or_default());
            log_trade(run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()),
                bundle: bundle_id.clone(), realized_usdc: None, error: None });
            let (ep, rd, owner, s, wh) = (endpoint.to_string(), run_dir.to_string(), cfg.authority.to_string(), sig, webhook.clone());
            let (be, bid) = (crank.block_engine.clone(), bundle_id);
            let tip_counter = daily_tip.clone();
            std::thread::spawn(move || {
                for wait in [5u64, 15, 45] {
                    std::thread::sleep(Duration::from_secs(wait));
                    if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                        *tip_counter.lock().unwrap() += tip_sol;
                        log_trade(&rd, &TradeLog { t: now(), liquidatee: String::new(), seize_native: 0,
                            est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()), bundle: None,
                            realized_usdc: Some(pnl), error: None });
                        alert(&wh, "liq-landed", &format!("liquidation landed {s}: realized ${pnl:.2}"));
                        return;
                    }
                }
                let status = bid.as_deref().and_then(|b| bundle_status(&be, b)).unwrap_or_default();
                alert(&wh, "liq-miss", &format!("liquidation {s} never confirmed (bundle status: {status})"));
            });
        }
        Err(e) => {
            eprintln!("[exec] send failed: {e}");
            log_trade(run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                est_profit_usdc: est_profit, tip_lamports, signature: None, bundle: None,
                realized_usdc: None, error: Some(e) });
        }
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let dry_run = std::env::var("DRY_RUN").map(|s| s != "0").unwrap_or(true);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let min_collateral: f64 = std::env::var("MIN_COLLATERAL_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let min_profit: f64 = std::env::var("MIN_PROFIT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(0.5);
    let tip_fraction_bps: u64 = std::env::var("TIP_FRACTION_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let min_tip_sol: f64 = std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let poll = Duration::from_millis(std::env::var("POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(5000));
    let rescan = Duration::from_secs(std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(300));
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.85);
    let slippage_bps: u32 = std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100);
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    // Helius Sender requires the tip go to one of ITS tip wallets.
    let tip_account = Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT")
        .unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap();
    let webhook = std::env::var("ALERT_WEBHOOK").ok();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();
    let tp = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap());

    // Optional Pyth Lazer pre-positioning: when PYTH_LAZER_TOKEN is set, blend
    // Lazer's ms-latency major prices over the on-chain oracle in the watch-set
    // recompute so the loop ARMS accounts about to cross the threshold ahead of
    // the on-chain crank. The FIRE decision stays gated by full on-chain sim —
    // Lazer only steers which accounts we spend sim budget on.
    let lazer_table = arb_engine::pyth::new_table();
    let lazer_map = arb_engine::lazer::mint_feed_map();
    let lazer_on = std::env::var("PYTH_LAZER_TOKEN").ok().filter(|t| !t.is_empty()).map(|token| {
        arb_engine::lazer::spawn_lazer_thread(token, arb_engine::lazer::arm_feed_ids(), lazer_table.clone());
        eprintln!("[exec] Pyth Lazer pre-positioning ENABLED");
    }).is_some();

    // Self-crank context: hot Hermes blob + Jito tip accounts. The edge only
    // triggers with Lazer on (that's what detects the true-price cross); the
    // fallback tip list keeps DRY_RUN sims working if the fetch fails —
    // DttWaMu… was observed live as the tip destination in the captured crank.
    let crank_on = std::env::var("CRANK").map(|s| s != "0").unwrap_or(true);
    let block_engine = default_block_engine();
    let tips: Vec<Pubkey> = if crank_on {
        get_tip_accounts(&block_engine).unwrap_or_default()
    } else { vec![] };
    let tips = if crank_on && tips.is_empty() {
        eprintln!("[exec] getTipAccounts failed — using fallback Jito tip list");
        vec![Pubkey::from_str("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL").unwrap()]
    } else { tips };
    let hermes_url = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let max_blob_ms: u64 = std::env::var("MAX_BLOB_AGE_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let crank = CrankCtx {
        on: crank_on,
        hermes: spawn_hermes_cache(hermes_url, vec![], Duration::from_millis(400)),
        tips,
        block_engine,
        max_blob_age: Duration::from_millis(max_blob_ms),
    };
    eprintln!("[exec] self-crank mode: {}", if crank.on { "ENABLED" } else { "off" });

    eprintln!("[exec] marginfi liquidation executor {}  authority={}  min_profit=${}  poll={:?} rescan={:?}  lazer={}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, min_profit, poll, rescan, lazer_on);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[exec] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor {wallet_min_sol}");
    }

    let mint_feed = arb_engine::lazer::mint_feed_map();
    let mut scan: Scan = full_scan(&endpoint).expect("initial scan");
    let mut last_scan = Instant::now();
    let mut watch: Vec<Pubkey> = Vec::new();
    let mut engine = arb_engine::liq_engine::Engine::new(min_collateral);
    // Counts only LANDED tips (a guard-reverted tx pays no tip — the ix
    // reverts with it), incremented by the readback thread.
    let daily_tip_sol = std::sync::Arc::new(std::sync::Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut mint_tp_cache: HashMap<Pubkey, Pubkey> = HashMap::new();
    // Ladder-rejected candidates (emode phantoms) re-sim at most once per
    // cooldown — they'd otherwise burn 5 gate sims every poll, forever.
    let sim_cooldown = Duration::from_secs(std::env::var("SIM_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(60));
    let mut sim_rejected: HashMap<Pubkey, Instant> = HashMap::new();
    // After handling a crossed account (fired or gated) don't re-process it for
    // this long — a persistently-crossed account would otherwise spin every tick.
    let handle_cooldown = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let mut handled: HashMap<Pubkey, Instant> = HashMap::new();
    let mut last_tick_us: u64 = 0;
    // Lazer-table poll granularity (ms). 1ms ≈ instant detection at negligible
    // CPU; raise it only to save CPU on a shared box.
    let tick_poll_ms: u64 = std::env::var("TICK_POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1);
    let mut first = true;

    let cfg = Cfg {
        liquidator_ma, authority, tp, usdc_bank, tip_account,
        tip_fraction_bps, min_tip_sol, min_profit, slippage_bps,
    };
    // Pre-built fire-tx cache: armed accounts (ratio ≥ ARM_RATIO) get a hot,
    // sim-verified tx so a cross → sign+send with no build/quote/sim on the
    // critical path. ARM_RATIO < 1.0 so the tx is ready BEFORE the cross.
    let arm_ratio: f64 = std::env::var("ARM_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.97);
    let arm_ttl = Duration::from_secs(std::env::var("ARM_TTL_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    // Per-cycle sim caps (bound + prioritize): with emode-aware health the
    // crossed sets are small, but a real crash could flag many at once — cap the
    // arm/fire work to the top-K by USD deficit so the sim budget always reaches
    // the biggest real opportunities first and never floods RPC or starves.
    let max_arm: usize = std::env::var("MAX_ARM_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(8);
    let max_fire: usize = std::env::var("MAX_FIRE_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(4);
    let mut cache: HashMap<Pubkey, CachedFire> = HashMap::new();
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);
    // Heartbeat cadence: the event-driven loop is otherwise silent between the
    // 5-min rescans, so a healthy-but-calm bot looks identical to a hung one or
    // a dead Lazer feed. HEARTBEAT_SECS=0 disables.
    let hb_every = std::env::var("HEARTBEAT_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30u64);
    let mut last_hb = Instant::now() - Duration::from_secs(9999);
    // How many crossed/arm-set accounts were deferred past the per-cycle cap
    // (surfaced in the heartbeat so a persistent backlog is visible).
    let mut fire_deferred = 0usize;
    let mut arm_deferred = 0usize;

    loop {
        // Refresh the watch-set + engine coefficients from a full scan.
        if first || last_scan.elapsed() >= rescan {
            if !first {
                if let Some(s) = full_scan(&endpoint) { scan = s; }
            }
            last_scan = Instant::now();
            let base = fresh_prices(&endpoint, &scan.oracle_of);
            let (prices, _led) = arb_engine::lazer::blend(&scan.banks, &base, &lazer_table, &lazer_map);
            // Only track accounts the fire path can act on (1 collateral / 1
            // USDC/USDT/wSOL debt); non-fireable shapes would otherwise inflate
            // the counts and starve deficit-ranking. Matches try_arm.
            let fireable: Vec<(Pubkey, MarginfiAccount)> = scan.accts.iter()
                .filter(|(_, a)| is_v1_fireable(a, &scan.banks))
                .cloned().collect();
            watch = fireable.iter().filter_map(|(pk, a)| {
                let r = liq::maintenance_health(a, &scan.banks, &prices);
                (r.missing == 0 && r.health.ratio() >= watch_ratio && r.health.weighted_assets >= min_collateral)
                    .then_some(*pk)
            }).collect();
            // Engine (event-driven trigger): coefficients over the on-chain
            // baseline; Lazer feeds move health between rescans with no RPC.
            let lazer_snapshot: HashMap<u32, f64> = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| Some((f, arb_engine::pyth::get(&lazer_table, f)?.price))).collect();
            let armed = engine.rebuild(&fireable, &scan.banks, &base, &mint_feed, &lazer_snapshot, watch_ratio);
            eprintln!("[exec] scan: {} borrowers → {} fireable-shaped → watch-set {} (ratio ≥ {}), engine armed {}",
                scan.accts.len(), fireable.len(), watch.len(), watch_ratio, armed);
            // Point the Hermes cache at the feeds we could actually need to
            // crank: crankable asset banks held by watch-set accounts.
            if crank.on {
                let watch_set: HashSet<Pubkey> = watch.iter().copied().collect();
                let feeds: HashSet<[u8; 32]> = scan.accts.iter()
                    .filter(|(pk, _)| watch_set.contains(pk))
                    .flat_map(|(_, a)| a.balances.iter())
                    .filter(|b| b.asset_shares > 0.0 && scan.crankable.contains(&b.bank_pk))
                    .filter_map(|b| scan.feed_of.get(&b.bank_pk).copied())
                    .collect();
                let hex: Vec<String> = feeds.iter()
                    .map(|f| f.iter().map(|x| format!("{x:02x}")).collect()).collect();
                eprintln!("[exec] crank: {} crankable banks, {} feeds in Hermes cache",
                    scan.crankable.len(), hex.len());
                crank.hermes.set_feeds(hex);
            }
            first = false;
        }

        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip_sol.lock().unwrap() = 0.0; }

        // Keep a recent blockhash hot so a fire stamps it without an RPC on the
        // critical path (refresh off the hot path, ~2s cadence).
        if last_bh.elapsed() >= Duration::from_secs(2) {
            if let Some(bh) = latest_blockhash(&endpoint) { fresh_bh = bh; last_bh = Instant::now(); }
        }

        // Trigger: event-driven on a Lazer tick (in-memory, no RPC) when the
        // feed is live; else fall back to the on-chain poll over the watch-set.
        let (to_eval, snap): (Vec<Pubkey>, HashMap<u32, f64>) = if lazer_on {
            let deadline = Instant::now() + poll;
            loop {
                let cur = arb_engine::lazer::arm_feed_ids().into_iter()
                    .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
                if cur > last_tick_us { last_tick_us = cur; break; }
                if Instant::now() >= deadline { break; }
                // Tight poll of the in-memory Lazer table (checking it is a few
                // µs). 1ms default cuts the tick→notice latency from ~10ms avg
                // (the old 20ms) to ~0.5ms — the biggest in-code detection win.
                std::thread::sleep(Duration::from_millis(tick_poll_ms));
            }
            let snap: HashMap<u32, f64> = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| Some((f, arb_engine::pyth::get(&lazer_table, f)?.price))).collect();
            // Rank crossed accounts by USD deficit, fire only the top MAX_FIRE
            // this cycle (deferred ones ride the next tick — deepest-underwater
            // first, so the biggest real opportunity is never starved).
            let ranked = engine.crossed_ranked(&snap, 1.0);
            fire_deferred = ranked.len().saturating_sub(max_fire);
            (ranked.into_iter().take(max_fire).map(|(pk, _)| pk).collect(), snap)
        } else {
            std::thread::sleep(poll);
            (watch.clone(), HashMap::new())
        };

        // Heartbeat: prove liveness + show how close the market is. `feeds live`
        // is the tell — if it's 0/N the Lazer WS is dead and the bot is inert
        // (every `crossed` returns empty), which otherwise looks just like calm.
        if lazer_on && hb_every > 0 && last_hb.elapsed() >= Duration::from_secs(hb_every) {
            let total_feeds = arb_engine::lazer::arm_feed_ids().len();
            let near = engine.crossed(&snap, arm_ratio).len();
            let crossing = engine.crossed(&snap, 1.0).len();
            let defer = if fire_deferred + arm_deferred > 0 {
                format!(" | DEFERRED fire {fire_deferred}/arm {arm_deferred} (raise MAX_*_PER_CYCLE)")
            } else { String::new() };
            // Detection freshness: how far behind the latest Lazer publish we are.
            let freshest = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            let lag_ms = now_us().saturating_sub(freshest as u128) / 1000;
            eprintln!("[hb] lazer feeds {}/{} live | detect_lag {}ms | {} within arm({}) | {} liquidatable now | cache {}{} | {}",
                snap.len(), total_feeds, lag_ms, near, arm_ratio, crossing, cache.len(), defer,
                arb_engine::lazer::status(&lazer_table));
            last_hb = Instant::now();
        }

        // ── ARM phase (lazer mode only): keep a hot, sim-verified fire tx for
        // accounts near the threshold (ratio ≥ arm_ratio) so the cross → send is
        // instant. Prune stale/no-longer-armed entries. Costs Jupiter quotes +
        // sims, but only for the small arm-set, and off the fire critical path.
        if lazer_on {
            // Ranked by USD deficit (closest-to-crossing first for the arm-set).
            let arm_ranked = engine.crossed_ranked(&snap, arm_ratio);
            let arm_keys: HashSet<Pubkey> = arm_ranked.iter().map(|(pk, _)| *pk).collect();
            // Drop cache entries that left the arm-set or went stale.
            cache.retain(|pk, c| arm_keys.contains(pk) && c.built.elapsed() < arm_ttl);
            let candidates: Vec<Pubkey> = arm_ranked.into_iter().map(|(pk, _)| pk)
                .filter(|pk| !cache.contains_key(pk))
                .filter(|pk| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown))
                .collect();
            // Cap the per-cycle arm work; the rest ride the next tick.
            arm_deferred = candidates.len().saturating_sub(max_arm);
            let need: Vec<Pubkey> = candidates.into_iter().take(max_arm).collect();
            if !need.is_empty() {
                let raw = get_multiple(&endpoint, &need);
                let base = fresh_prices(&endpoint, &scan.oracle_of);
                let (prices, _) = arb_engine::lazer::blend(&scan.banks, &base, &lazer_table, &lazer_map);
                for pk in &need {
                    let Some(a) = raw.get(pk).and_then(|r| MarginfiAccount::decode(r)) else { continue };
                    match try_arm(&endpoint, &run_dir, &cfg, &crank, &scan, &a, pk, &prices, &base, &mut mint_tp_cache) {
                        Some(c) => { cache.insert(*pk, c); }
                        None => { sim_rejected.insert(*pk, Instant::now()); }
                    }
                }
            }
        }

        // Drop accounts handled recently (avoid per-tick spin on a standing cross).
        let to_eval: Vec<Pubkey> = to_eval.into_iter()
            .filter(|pk| handled.get(pk).is_none_or(|t| t.elapsed() >= handle_cooldown))
            .collect();
        if to_eval.is_empty() { continue; }

        // ── FIRE phase: for each crossed account, prefer the armed cache
        // (instant); else arm it inline now (covers a cross that outran the arm
        // pass, and the whole poll-mode path). Then send.
        let fresh_raw = get_multiple(&endpoint, &to_eval);
        let base = fresh_prices(&endpoint, &scan.oracle_of);
        let (prices, _lazer_led) = arb_engine::lazer::blend(&scan.banks, &base, &lazer_table, &lazer_map);
        for pk in &to_eval {
            handled.insert(*pk, Instant::now());
            let cached = match cache.remove(pk).filter(|c| c.built.elapsed() < arm_ttl) {
                Some(c) => Some(c),
                None => {
                    // Not armed (or stale) — build inline now.
                    let Some(a) = fresh_raw.get(pk).and_then(|r| MarginfiAccount::decode(r)) else { continue };
                    let r = liq::maintenance_health(&a, &scan.banks, &prices);
                    if r.missing > 0 || !r.health.liquidatable() || r.health.weighted_assets < min_collateral { continue; }
                    match try_arm(&endpoint, &run_dir, &cfg, &crank, &scan, &a, pk, &prices, &base, &mut mint_tp_cache) {
                        Some(c) => Some(c),
                        None => { sim_rejected.insert(*pk, Instant::now()); None }
                    }
                }
            };
            if let Some(c) = cached {
                let armed_from_cache = c.built.elapsed().as_millis() < arm_ttl.as_millis() && c.built.elapsed().as_millis() > 0;
                let fire_start = now_us();
                fire_cached(&endpoint, &run_dir, &sender_url, &cfg, &crank, dry_run, pk, &c, fresh_bh,
                    kp.as_ref(), &daily_tip_sol, max_daily_tip_sol, wallet_min_sol, &webhook);
                // Latency ledger: from the Lazer publish that made this cross
                // (last_tick_us) to detection (loop) to fire submit. Proves
                // whether we can act inside the liquidation window.
                let done = now_us();
                log_latency(&run_dir, &serde_json::json!({
                    "t": now(), "account": pk.to_string(), "mode": c.mode.name(),
                    "appeared_us": last_tick_us,
                    "detected_lag_ms": fire_start.saturating_sub(last_tick_us as u128) / 1000,
                    "submit_lag_ms": done.saturating_sub(last_tick_us as u128) / 1000,
                    "fire_submit_ms": done.saturating_sub(fire_start) / 1000,
                    "armed": armed_from_cache, "dry_run": dry_run,
                }));
            }
        }
    }
}
