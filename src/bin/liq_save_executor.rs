//! Production Save (Solend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//!
//! The old build polled stored on-chain health every 30s. That lost every race:
//! the census found 45 USDC-debt Solend liquidations in 48h and we caught 0,
//! because competitors react to the oracle in milliseconds. This rewrite mirrors
//! the marginfi executor's architecture — a Lazer WebSocket feeds an in-memory
//! health engine (src/save_engine.rs) that recomputes every obligation's
//! borrowed/unhealthy on each ~ms price tick with ZERO RPC, so a cross is
//! noticed in ~ms not ~30s.
//!
//!   full scan (RESCAN_SECS): v1 (1 collateral / 1 debt, debt ∈ {USDC,USDT,wSOL}) obligations →
//!     save_engine watch-set (stored on-chain health + per-side Lazer anchors)
//!   Lazer tick (TICK_POLL_MS in-memory poll): the trigger to RE-CHECK, not the
//!     liquidatable verdict — Lazer leads/diverges from the on-chain Pyth price
//!   FIRE tier (TWO-TIER GATING): Lazer NARROWS the watch-set; the ON-CHAIN
//!     oracle price GATES the expensive sim. Only obligations liquidatable at the
//!     on-chain price Solend settles against (stored health from the last rescan,
//!     ZERO Lazer projection) earn a sim, ranked by USD deficit, capped top-K
//!     (MAX_FIRE_PER_CYCLE). Gating on the Lazer-projected ratio instead flooded
//!     ~390 phantoms/cycle through simulateTransaction/Bundle (healthy on-chain).
//!   ARM those FIRE-tier candidates: pre-build+size+sim the fire tx → hot cache
//!   FIRE on tick: stamp fresh blockhash, sign, submit (no build/quote/sim on
//!     the critical path)
//!
//! Two fire modes, exactly like marginfi:
//!   Sender — obligation already liquidatable at ON-CHAIN prices → single tx via
//!     Helius Sender.
//!   Crank  — underwater at the true (Lazer) price but Solend hasn't cranked its
//!     Pyth feed yet → atomic Jito bundle [crank_setup, crank_fire, fire] that
//!     posts the fresh price then liquidates. Save reserves read the SAME shard-0
//!     sponsored feeds we crank, so refresh_reserve inside the fire tx picks up
//!     the cranked price. Sizing + ground truth run through simulateBundle.
//!
//! Profit-or-revert (payback_all fails unless the swap covered the borrow), so a
//! losing fire that lands costs only the base fee; a failing bundle never lands.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [PYTH_LAZER_TOKEN=… (required for event-driven + crank)] [CRANK=1]
//!        [MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [REPAY_FRACS=0.2,0.1,0.05]
//!        [WATCH_RATIO=0.85] [ARM_RATIO=0.97] [RESCAN_SECS=30] [TICK_POLL_MS=1]
//!        [MAX_ARM_PER_CYCLE=8] [MAX_FIRE_PER_CYCLE=4] [SLIPPAGE_BPS=100]
//!        [MAX_SWAP_ACCOUNTS=18] [MAX_BLOB_AGE_MS=3000] cargo run --release --bin liq_save_executor

use arb_engine::jito::{bundle_status, default_block_engine, get_tip_accounts, send_bundle, send_sender};
use arb_engine::liquidation as liq;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use arb_engine::pyth_accumulator::{spawn_hermes_cache, HermesCache};
use arb_engine::pyth_crank;
use arb_engine::save::{self, Obligation, Reserve};
use arb_engine::save_engine::Engine;
use arb_engine::save_fire::{build_save_fire_tx, SaveFireCandidate};
use serde::Serialize;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
/// Classic SPL token program — every Save main-pool debt mint (USDC/USDT/wSOL).
const CLASSIC_TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
/// Pyth Lazer USDT/USD numeric feed id (verified against the Lazer symbol
/// registry; consistent with the codebase's SOL=6 / USDC=7). wSOL debt already
/// maps to the SOL feed (6). Added to the executor's local feed set so USDT-debt
/// obligations are subscribed + tracked without editing the shared lazer map.
const LAZER_USDT: u32 = 8;

/// Feed ids the executor subscribes/snapshots: the shared majors + USDT.
fn arm_feeds() -> Vec<u32> {
    let mut v = arb_engine::lazer::arm_feed_ids();
    if !v.contains(&LAZER_USDT) { v.push(LAZER_USDT); }
    v
}
/// mint → Lazer feed, the shared map extended with USDT (→ feed 8) so a USDT
/// debt side is priced by Lazer like USDC is.
fn mint_feed_ext() -> HashMap<Pubkey, u32> {
    let mut m = arb_engine::lazer::mint_feed_map();
    m.insert(Pubkey::from_str(save::USDT_MINT).unwrap(), LAZER_USDT);
    m
}

fn now() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }
fn now_us() -> u128 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_micros() }

/// Latency ledger — proves whether SPEED is (still) the bottleneck. `appeared_us`
/// is the Lazer PUBLISH timestamp of the tick that made the obligation cross; the
/// deltas measure detect + submit lag from that instant. → {run_dir}/latency.jsonl
fn log_latency(run_dir: &str, v: &serde_json::Value) {
    let _ = std::fs::create_dir_all(run_dir);
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(format!("{run_dir}/latency.jsonl")) {
        use std::io::Write;
        let _ = writeln!(f, "{v}");
    }
}

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn b64tx(tx: &VersionedTransaction) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx).unwrap())
}
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
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
fn simulate_ok(endpoint: &str, b64: &str) -> bool {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))
        .and_then(|v| v["result"].get("value").map(|val| val["err"].is_null())).unwrap_or(false)
}
/// How many leading txs of a bundle succeed (jito stops at the first revert).
/// For [setup, fire, save_fire] `ran_ok == 3` = accepted, `< 2` = crank broke.
fn simulate_bundle_ran_ok(endpoint: &str, txs_b64: &[String]) -> Option<usize> {
    let nulls = vec![serde_json::Value::Null; txs_b64.len()];
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateBundle",
        "params":[{"encodedTransactions": txs_b64}, {
            "skipSigVerify": true, "replaceRecentBlockhash": true,
            "preExecutionAccountsConfigs": nulls, "postExecutionAccountsConfigs": nulls,
        }]}))?;
    if v.get("error").filter(|e| !e.is_null()).is_some() { return None; }
    let results = v["result"]["value"]["transactionResults"].as_array().cloned().unwrap_or_default();
    Some(results.iter().take_while(|r| r["err"].is_null()).count())
}

#[derive(Serialize)]
struct DecisionLog {
    t: u64, obligation: String, protocol: &'static str, mode: String, debt_usd: f64, ratio: f64,
    repay_native: u64, quoted_usdc_out: f64, est_profit_usdc: f64, fired: bool, reason: String,
}
#[derive(Serialize)]
struct TradeLog {
    t: u64, obligation: String, protocol: &'static str, repay_native: u64, est_profit_usdc: f64,
    tip_lamports: u64, signature: Option<String>, bundle: Option<String>, realized_usdc: Option<f64>, error: Option<String>,
}

/// How a cached fire gets submitted.
#[derive(Clone)]
enum FireMode {
    /// Single tx via Helius Sender — liquidatable at on-chain prices already.
    Sender,
    /// Jito bundle [crank_setup, crank_fire, save_fire] — underwater at the true
    /// (Lazer) price only; the crank posts the fresh price for refresh_reserve.
    Crank { feed_id: [u8; 32] },
}
impl FireMode {
    fn name(&self) -> &'static str { match self { FireMode::Sender => "sender", FireMode::Crank { .. } => "crank" } }
}

/// Everything the crank path needs, spun up once at boot (shared with marginfi's design).
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

/// A full scan: v1 accepted-debt (USDC/USDT/wSOL) obligations + the
/// reserves/oracle metadata they touch.
struct SaveScan {
    obls: Vec<(Pubkey, Obligation)>,
    reserves: HashMap<Pubkey, Reserve>,      // collateral reserves (+ the debt reserves)
    ctp_of: HashMap<Pubkey, Pubkey>,         // collateral liquidity mint → token program
    feed_of: HashMap<Pubkey, [u8; 32]>,      // collateral reserve → 32-byte Pyth feed id
    crankable: HashSet<Pubkey>,              // collateral reserves whose pyth_oracle is the shard-0 sponsored PDA
}

/// Scan obligations (full), keep v1 / debt in {USDC,USDT,wSOL} / ≥ min_debt, then
/// load their collateral reserves + oracle crank metadata. The debt reserves are
/// passed in pre-decoded (stable accounts).
fn full_scan_save(
    endpoint: &str, debt_reserves: &HashMap<Pubkey, Reserve>, min_debt: f64,
    ctp_cache: &mut HashMap<Pubkey, Pubkey>,
) -> Option<SaveScan> {
    // One getProgramAccounts per scanned pool (memcmp matches a single value),
    // merged. The obligation's own lending_market flows through to the fire tx,
    // so multi-pool needs no fire-path change — just these obligations + the
    // pools' debt reserves in `debt_reserves`.
    let mut entries: Vec<serde_json::Value> = Vec::new();
    for pool in save::SCAN_POOLS {
        let Some(resp) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
            "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSize":1300,
                "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":*pool}}]}]})) else { continue };
        if let Some(arr) = resp["result"].as_array() { entries.extend(arr.iter().cloned()); }
    }
    if entries.is_empty() { return None; }
    let mut obls: Vec<(Pubkey, Obligation)> = Vec::new();
    for e in &entries {
        let Some(pk) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(d) = b64(&e["account"]["data"]) else { continue };
        let Some(o) = Obligation::decode(&d) else { continue };
        if o.deposits.len() != 1 || o.borrows.len() != 1 { continue; }   // v1 shape (fire path)
        if !debt_reserves.contains_key(&o.borrows[0].reserve) { continue; } // accepted debt only
        if o.borrowed_value < min_debt { continue; }
        obls.push((pk, o));
    }
    // Load the distinct collateral reserves referenced.
    let coll_pks: Vec<Pubkey> = obls.iter().map(|(_, o)| o.deposits[0].reserve).collect::<HashSet<_>>().into_iter().collect();
    let mut reserves: HashMap<Pubkey, Reserve> = debt_reserves.clone();
    for (pk, raw) in &get_multiple(endpoint, &coll_pks) {
        if let Some(r) = Reserve::decode(*pk, raw) { reserves.insert(*pk, r); }
    }
    // Collateral-mint → token program (for the redeem ATA).
    let mut ctp_of = HashMap::new();
    for pk in &coll_pks {
        if let Some(r) = reserves.get(pk) {
            let tp = match ctp_cache.get(&r.liquidity_mint) {
                Some(t) => *t,
                None => match mint_owner(endpoint, &r.liquidity_mint) { Some(t) => { ctp_cache.insert(r.liquidity_mint, t); t }, None => continue },
            };
            ctp_of.insert(r.liquidity_mint, tp);
        }
    }
    // Oracle crank metadata: decode each collateral reserve's pyth_oracle → feed
    // id, and mark crankable when the oracle IS that feed's shard-0 sponsored PDA.
    let oracle_pks: Vec<Pubkey> = coll_pks.iter().filter_map(|pk| reserves.get(pk).map(|r| r.pyth_oracle)).collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(endpoint, &oracle_pks);
    let mut feed_of = HashMap::new();
    let mut crankable = HashSet::new();
    for pk in &coll_pks {
        let Some(r) = reserves.get(pk) else { continue };
        let Some((fid, _, _)) = oracle_raw.get(&r.pyth_oracle).and_then(|raw| liq::decode_price_update_v2(raw)) else { continue };
        feed_of.insert(*pk, fid);
        if pyth_crank::sponsored_feed(0, &fid) == r.pyth_oracle { crankable.insert(*pk); }
    }
    Some(SaveScan { obls, reserves, ctp_of, feed_of, crankable })
}

#[derive(Clone, Copy)]
struct Cfg {
    authority: Pubkey,
    tip_account: Pubkey,
    tip_fraction_bps: u64,
    min_tip_sol: f64,
    min_profit: f64,
    slippage_bps: u32,
    max_swap_accounts: usize,
}

/// A sim-verified fire tx kept hot for an armed obligation. Compiled with a
/// placeholder blockhash (sim replaces it); the real hash is stamped at fire.
#[derive(Clone)]
struct CachedFire {
    tx: VersionedTransaction,
    mode: FireMode,
    tip_lamports: u64,
    tip_sol: f64,
    est_profit: f64,
    repay: u64,
    debt_usd: f64,
    ratio: f64,
    built: Instant,
}

/// Build + size + profit-gate + full-sim-gate one obligation → CachedFire.
/// Mirrors the marginfi try_arm: mode from on-chain vs Lazer health, size by a
/// sim ladder (bundle sim in crank mode so the chain judges at the cranked
/// price), profit gate, ground-truth sim. This is the only place a fire tx is
/// built; the sim lives here (arm time), off the fire critical path.
#[allow(clippy::too_many_arguments)]
fn try_arm(
    endpoint: &str, run_dir: &str, cfg: &Cfg, crank: &CrankCtx, scan: &SaveScan,
    pk: &Pubkey, repay_fracs: &[f64], engine_ratio: f64,
) -> Option<CachedFire> {
    let log_skip = |mode: &str, debt: f64, ratio: f64, reason: &str| {
        log_decision(run_dir, &DecisionLog {
            t: now(), obligation: pk.to_string(), protocol: "save", mode: mode.into(),
            debt_usd: debt, ratio, repay_native: 0, quoted_usdc_out: 0.0, est_profit_usdc: 0.0,
            fired: false, reason: reason.into(),
        });
    };
    // Fresh obligation (health may have moved since scan) + its collateral reserve.
    let o = get_acct(endpoint, pk).and_then(|d| Obligation::decode(&d))?;
    if o.deposits.len() != 1 || o.borrows.len() != 1 { return None; }
    let coll_pk = o.deposits[0].reserve;
    let coll = scan.reserves.get(&coll_pk)?.clone();
    let ctp = *scan.ctp_of.get(&coll.liquidity_mint)?;
    // The obligation's actual debt reserve (USDC/USDT/wSOL) — prices the repay
    // and is the flash-borrow/swap-target asset.
    let debt_reserve = scan.reserves.get(&o.borrows[0].reserve)?.clone();
    let debt_dec = 10f64.powi(debt_reserve.mint_decimals as i32);
    let debt_tp = Pubkey::from_str(CLASSIC_TOKEN_PROGRAM).unwrap();
    let debt_usd = o.borrowed_value;

    // Mode: liquidatable at FRESH on-chain prices (the value Solend's `liquidate`
    // recomputes at settle time — same cToken-exchange-rate math the fire tier
    // gates on, so routing stays consistent and never drops a genuine fire) →
    // Sender. Else the Lazer engine flagged it but Solend hasn't cranked yet →
    // crank + liquidate bundle (needs a crankable oracle + a fresh Hermes blob).
    let mode = if o.fresh_liquidatable(&scan.reserves) {
        FireMode::Sender
    } else {
        if !crank.on { return None; }
        if !scan.crankable.contains(&coll_pk) {
            log_skip("crank", debt_usd, engine_ratio, "flagged at Lazer price but healthy on-chain and collateral oracle not crankable — cannot act");
            return None;
        }
        let Some(feed_id) = scan.feed_of.get(&coll_pk).copied() else {
            log_skip("crank", debt_usd, engine_ratio, "crankable but feed id missing"); return None;
        };
        if crank.hermes.update_for(&feed_id).is_none() {
            log_skip("crank", debt_usd, engine_ratio, "crankable but no fresh Hermes blob for feed yet"); return None;
        }
        FireMode::Crank { feed_id }
    };

    // Crank txs for the sizing/ground-truth bundle (placeholder blockhash — sims
    // replace it; the LIVE fire rebuilds from the freshest blob).
    let crank_b64: Option<(String, String)> = match &mode {
        FireMode::Sender => None,
        FireMode::Crank { feed_id } => {
            let (mu, vaa, _age) = crank.hermes.update_for(feed_id)?;
            let txs = pyth_crank::build_crank_txs(&cfg.authority, &vaa, std::slice::from_ref(&mu), 0, 0, solana_hash::Hash::default()).ok()?;
            Some(txs.to_b64().ok()?)
        }
    };

    // The in-tx tip destination differs by mode: a Sender fire tips a Helius
    // Sender wallet; a crank fire rides a Jito bundle and must tip a Jito account.
    let tip_to = match &mode {
        FireMode::Sender => cfg.tip_account,
        FireMode::Crank { .. } => match crank.pick_tip() {
            Some(t) => t,
            None => { log_skip("crank", debt_usd, engine_ratio, "no Jito tip accounts"); return None; }
        },
    };
    let mk = |repay: u64, seize: u64, tip: u64, bh: solana_hash::Hash| {
        let c = SaveFireCandidate {
            obligation: *pk, repay_reserve: debt_reserve.clone(), withdraw_reserve: coll.clone(),
            collateral_token_program: ctp, debt_token_program: debt_tp, repay_amount: repay, seize_underlying: seize,
            deposit_reserves: vec![coll.reserve], borrow_reserves: vec![debt_reserve.reserve],
        };
        build_save_fire_tx(endpoint, &c, &cfg.authority,
            Some(tip_to), tip, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, bh).ok()
    };
    // gate: standalone sim (Sender) or bundle sim (crank) so the chain judges at
    // the actionable price.
    let gate = |fire: &VersionedTransaction| -> bool {
        match &crank_b64 {
            None => simulate_ok(endpoint, &b64tx(fire)),
            Some((s, f)) => simulate_bundle_ran_ok(endpoint, &[s.clone(), f.clone(), b64tx(fire)]) == Some(3),
        }
    };

    // Size by simulation ladder — largest repay fraction Solend accepts.
    let ph = solana_hash::Hash::default();
    let mut chosen: Option<(u64, arb_engine::save_fire::SaveFireTx)> = None;
    for frac in repay_fracs {
        let repay_usd = debt_usd * frac;
        let repay = (repay_usd / debt_reserve.market_price.max(1e-9) * debt_dec).max(1.0) as u64;
        let seized_usd = repay_usd * (1.0 + coll.liquidation_bonus_pct as f64 / 100.0);
        let seize = (seized_usd / coll.market_price.max(1e-9) * 10f64.powi(coll.mint_decimals as i32)) as u64;
        let Some(fire) = mk(repay, seize, 0, ph) else { continue };
        if gate(&fire.tx) { chosen = Some((repay, fire)); break; }
    }
    let Some((repay, fire)) = chosen else {
        log_skip(mode.name(), debt_usd, engine_ratio, "no repay fraction passed sim (healthy at actionable price / too small)");
        return None;
    };

    // Profit gate — price both legs in the debt asset's decimals + market price.
    let repay_usd = repay as f64 / debt_dec * debt_reserve.market_price;
    let usdc_out = fire.quoted_debt_out as f64 / debt_dec * debt_reserve.market_price;
    let est_profit = usdc_out - repay_usd;
    let sol_usd = 150.0; // conservative; tip is tiny vs profit
    let tip_sol = (est_profit * cfg.tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(cfg.min_tip_sol);
    let tip_lamports = (tip_sol * 1e9) as u64;
    let mut log = DecisionLog {
        t: now(), obligation: pk.to_string(), protocol: "save", mode: mode.name().into(),
        debt_usd, ratio: engine_ratio, repay_native: repay, quoted_usdc_out: usdc_out,
        est_profit_usdc: est_profit, fired: false, reason: String::new(),
    };
    if est_profit < cfg.min_profit + tip_sol * sol_usd {
        log.reason = format!("below min profit (est ${est_profit:.2})"); log_decision(run_dir, &log); return None;
    }

    // Final build WITH the tip, ground-truth sim gate.
    let seized_usd = repay_usd * (1.0 + coll.liquidation_bonus_pct as f64 / 100.0);
    let seize = (seized_usd / coll.market_price.max(1e-9) * 10f64.powi(coll.mint_decimals as i32)) as u64;
    let Some(fire) = mk(repay, seize, tip_lamports, ph) else {
        log.reason = "final build failed".into(); log_decision(run_dir, &log); return None;
    };
    if !gate(&fire.tx) {
        log.reason = "final fire sim revert (swap/repay would not cover the borrow)".into(); log_decision(run_dir, &log); return None;
    }
    Some(CachedFire { tx: fire.tx, mode, tip_lamports, tip_sol, est_profit, repay, debt_usd, ratio: engine_ratio, built: Instant::now() })
}

/// Fire a cached tx: stamp fresh blockhash, sign, submit (Sender or a Jito
/// bundle with freshly-built crank txs), log, spawn P&L readback.
#[allow(clippy::too_many_arguments)]
fn fire_cached(
    endpoint: &str, run_dir: &str, sender_url: &str, cfg: &Cfg, crank: &CrankCtx, dry_run: bool,
    pk: &Pubkey, cached: &CachedFire, fresh_bh: solana_hash::Hash, kp: Option<&Keypair>,
    daily_tip: &std::sync::Arc<std::sync::Mutex<f64>>, max_daily_tip: f64, wallet_min: f64,
    webhook: &Option<String>,
) {
    let mode = cached.mode.name();
    let mut log = DecisionLog {
        t: now(), obligation: pk.to_string(), protocol: "save", mode: mode.into(), debt_usd: cached.debt_usd,
        ratio: cached.ratio, repay_native: cached.repay, quoted_usdc_out: 0.0, est_profit_usdc: cached.est_profit,
        fired: false, reason: String::new(),
    };
    println!("★ SAVE LIQUIDATABLE [{mode}]  {}  debt ${:.0}  repay {}  est profit ${:.2}  tip {:.5} SOL  (armed {:?} ago)",
        &pk.to_string()[..8], cached.debt_usd, cached.repay, cached.est_profit, cached.tip_sol, cached.built.elapsed());
    if dry_run {
        log.reason = format!("dry-run: would fire ({mode}, armed)"); log_decision(run_dir, &log);
        alert(webhook, "save-dry", &format!("DRY-RUN Save {mode} liquidation {} est profit ${:.2}", pk, cached.est_profit));
        return;
    }
    if *daily_tip.lock().unwrap() + cached.tip_sol > max_daily_tip {
        log.reason = "daily tip cap".into(); log_decision(run_dir, &log);
        alert(webhook, "save-cap", "daily tip cap reached"); return;
    }
    if sol_balance(endpoint, &cfg.authority.to_string()) < wallet_min {
        log.reason = "wallet below floor".into(); log_decision(run_dir, &log);
        alert(webhook, "save-floor", "wallet below floor — not firing"); return;
    }
    let mut tx = cached.tx.clone();
    tx.message.set_recent_blockhash(fresh_bh);
    let kp = kp.unwrap();
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    let tx_b64 = b64tx(&tx);
    let (repay, est_profit, tip_lamports, tip_sol) = (cached.repay, cached.est_profit, cached.tip_lamports, cached.tip_sol);

    let submit: Result<Option<String>, String> = match &cached.mode {
        FireMode::Sender => send_sender(sender_url, &tx_b64).map(|_| None).map_err(|e| e.to_string()),
        FireMode::Crank { feed_id } => (|| {
            let (mu, vaa, age) = crank.hermes.update_for(feed_id).ok_or_else(|| "no Hermes blob for feed".to_string())?;
            if age > crank.max_blob_age { return Err(format!("Hermes blob stale ({age:?}) — not bundling")); }
            let mut ctxs = pyth_crank::build_crank_txs(&cfg.authority, &vaa, std::slice::from_ref(&mu), 0, 0, fresh_bh).map_err(|e| e.to_string())?;
            ctxs.stamp_and_sign(kp, fresh_bh);
            let (setup_b64, crank_b64) = ctxs.to_b64().map_err(|e| e.to_string())?;
            let mut last = String::new();
            for attempt in 0..3 {
                match send_bundle(&crank.block_engine, &[setup_b64.clone(), crank_b64.clone(), tx_b64.clone()]) {
                    Ok(id) => return Ok(Some(id)),
                    Err(e) if e.to_string().contains("429") && attempt < 2 => { last = e.to_string(); std::thread::sleep(Duration::from_millis(250)); }
                    Err(e) => return Err(e.to_string()),
                }
            }
            Err(last)
        })(),
    };

    log.fired = submit.is_ok(); log.reason = format!("fired ({mode}, armed cache)"); log_decision(run_dir, &log);
    match submit {
        Ok(bundle_id) => {
            eprintln!("[save] FIRED [{mode}] {sig}{}", bundle_id.as_deref().map(|b| format!(" bundle {b}")).unwrap_or_default());
            log_trade(run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "save", repay_native: repay,
                est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), bundle: bundle_id.clone(), realized_usdc: None, error: None });
            let (ep, rd, owner, s, wh) = (endpoint.to_string(), run_dir.to_string(), cfg.authority.to_string(), sig, webhook.clone());
            let (be, bid, tc) = (crank.block_engine.clone(), bundle_id, daily_tip.clone());
            std::thread::spawn(move || {
                for wait in [5u64, 15, 45] {
                    std::thread::sleep(Duration::from_secs(wait));
                    if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                        *tc.lock().unwrap() += tip_sol;
                        log_trade(&rd, &TradeLog { t: now(), obligation: String::new(), protocol: "save", repay_native: 0,
                            est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()), bundle: None, realized_usdc: Some(pnl), error: None });
                        alert(&wh, "save-landed", &format!("Save liquidation landed {s}: realized ${pnl:.2}"));
                        return;
                    }
                }
                let status = bid.as_deref().and_then(|b| bundle_status(&be, b)).unwrap_or_default();
                alert(&wh, "save-miss", &format!("Save liquidation {s} never confirmed (bundle status: {status})"));
            });
        }
        Err(e) => {
            eprintln!("[save] send failed: {e}");
            log_trade(run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "save", repay_native: repay,
                est_profit_usdc: est_profit, tip_lamports, signature: None, bundle: None, realized_usdc: None, error: Some(e) });
        }
    }
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let dry_run = std::env::var("DRY_RUN").map(|s| s != "0").unwrap_or(true);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let min_debt: f64 = std::env::var("MIN_DEBT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let ratio_cap: f64 = std::env::var("RATIO_CAP").ok().and_then(|s| s.parse().ok()).unwrap_or(3.0);
    let min_profit: f64 = std::env::var("MIN_PROFIT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(0.5);
    let rescan = Duration::from_secs(std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30));
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.85);
    let arm_ratio: f64 = std::env::var("ARM_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.97);
    let arm_ttl = Duration::from_secs(std::env::var("ARM_TTL_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let max_arm: usize = std::env::var("MAX_ARM_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(8);
    let max_fire: usize = std::env::var("MAX_FIRE_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(4);
    let tick_poll_ms: u64 = std::env::var("TICK_POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1);
    let poll = Duration::from_millis(std::env::var("POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(5000));
    let sim_cooldown = Duration::from_secs(std::env::var("SIM_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(60));
    let handle_cooldown = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let hb_every = std::env::var("HEARTBEAT_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30u64);
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let webhook = std::env::var("ALERT_WEBHOOK").ok();
    let repay_fracs: Vec<f64> = std::env::var("REPAY_FRACS").ok()
        .map(|s| s.split(',').filter_map(|x| x.trim().parse().ok()).collect())
        .unwrap_or_else(|| vec![0.2, 0.1, 0.05]);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);

    let cfg = Cfg {
        authority: Pubkey::default(), // set after keypair
        tip_account: Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT").unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap(),
        tip_fraction_bps: std::env::var("TIP_FRACTION_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000),
        min_tip_sol: std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002),
        min_profit,
        slippage_bps: std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100),
        max_swap_accounts: std::env::var("MAX_SWAP_ACCOUNTS").ok().and_then(|s| s.parse().ok()).unwrap_or(18),
    };

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap());
    let cfg = Cfg { authority, ..cfg };

    // Lazer WebSocket: the event-driven trigger. Without a token the loop still
    // runs but only on the slow poll fallback — warn loudly, since that's the
    // exact 30s-poll regression this rewrite exists to kill.
    let lazer_table = arb_engine::pyth::new_table();
    let mint_feed = mint_feed_ext();
    let lazer_on = std::env::var("PYTH_LAZER_TOKEN").ok().filter(|t| !t.is_empty()).map(|token| {
        arb_engine::lazer::spawn_lazer_thread(token, arm_feeds(), lazer_table.clone());
        eprintln!("[save] Pyth Lazer event-driven trigger ENABLED");
    }).is_some();
    if !lazer_on { eprintln!("[save] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the 30s regression). Set the token for ms detection."); }

    // Crank context (front-run Solend's own cranker on stale feeds).
    let crank_on = std::env::var("CRANK").map(|s| s != "0").unwrap_or(true) && lazer_on;
    let block_engine = default_block_engine();
    let tips: Vec<Pubkey> = if crank_on { get_tip_accounts(&block_engine).unwrap_or_default() } else { vec![] };
    let tips = if crank_on && tips.is_empty() {
        eprintln!("[save] getTipAccounts failed — using fallback Jito tip list");
        vec![Pubkey::from_str("DttWaMuVvTiduZRnguLF7jNxTgiMBZ1hyAumKUiL2KRL").unwrap()]
    } else { tips };
    let hermes_url = std::env::var("HERMES").unwrap_or_else(|_| "https://hermes.pyth.network".into());
    let max_blob_ms: u64 = std::env::var("MAX_BLOB_AGE_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let crank = CrankCtx {
        on: crank_on,
        hermes: spawn_hermes_cache(hermes_url, vec![], Duration::from_millis(400)),
        tips, block_engine, max_blob_age: Duration::from_millis(max_blob_ms),
    };

    // Debt reserves decoded once (stable accounts). Each has a wired JupLend
    // flash market and is what the fire path repays: USDC/USDT/wSOL.
    let mut debt_reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    for res in save::DEBT_RESERVES.iter().copied() {
        let pk = Pubkey::from_str(res).unwrap();
        let r = Reserve::decode(pk, &get_acct(&endpoint, &pk).unwrap_or_else(|| panic!("fetch debt reserve {res}")))
            .unwrap_or_else(|| panic!("decode debt reserve {res}"));
        debt_reserves.insert(pk, r);
    }

    eprintln!("[save] Solend liquidation executor {}  authority={}  min_debt=${min_debt} rescan={:?} tick_poll={}ms lazer={} crank={}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, rescan, tick_poll_ms, lazer_on, crank.on);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[save] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor");
    }

    let mut engine = Engine::new(min_debt, ratio_cap);
    let mut ctp_cache: HashMap<Pubkey, Pubkey> = HashMap::new();
    let mut scan = full_scan_save(&endpoint, &debt_reserves, min_debt, &mut ctp_cache).expect("initial scan");
    let mut last_scan = Instant::now();

    let daily_tip = std::sync::Arc::new(std::sync::Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);
    let mut handled: HashMap<Pubkey, Instant> = HashMap::new();
    let mut sim_rejected: HashMap<Pubkey, Instant> = HashMap::new();
    let mut cache: HashMap<Pubkey, CachedFire> = HashMap::new();
    let mut last_tick_us: u64 = 0;
    let mut last_hb = Instant::now() - Duration::from_secs(9999);
    let mut arm_deferred = 0usize;
    let mut first = true;

    let lazer_snapshot = |table: &arb_engine::pyth::PriceTable| -> HashMap<u32, f64> {
        arm_feeds().into_iter()
            .filter_map(|f| Some((f, arb_engine::pyth::get(table, f)?.price))).collect()
    };

    loop {
        // Rebuild the watch-set + engine from a full scan.
        if first || last_scan.elapsed() >= rescan {
            if !first {
                if let Some(s) = full_scan_save(&endpoint, &debt_reserves, min_debt, &mut ctp_cache) { scan = s; }
            }
            last_scan = Instant::now();
            let snap = lazer_snapshot(&lazer_table);
            let armed = engine.rebuild(&scan.obls, &scan.reserves, &mint_feed, watch_ratio, &snap);
            eprintln!("[save] scan: {} v1 USDC/USDT/wSOL-debt obligations (≥ ${min_debt}) → engine watch-set {} (ratio ≥ {})",
                scan.obls.len(), armed, watch_ratio);
            if crank.on {
                let feeds: HashSet<[u8; 32]> = engine.accounts.iter()
                    .filter(|w| scan.crankable.contains(&w.coll_reserve))
                    .filter_map(|w| scan.feed_of.get(&w.coll_reserve).copied()).collect();
                let hex: Vec<String> = feeds.iter().map(|f| f.iter().map(|x| format!("{x:02x}")).collect()).collect();
                eprintln!("[save] crank: {} crankable collateral reserves, {} feeds in Hermes cache", scan.crankable.len(), hex.len());
                crank.hermes.set_feeds(hex);
            }
            first = false;
        }

        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip.lock().unwrap() = 0.0; }
        if last_bh.elapsed() >= Duration::from_secs(2) {
            if let Some(bh) = latest_blockhash(&endpoint) { fresh_bh = bh; last_bh = Instant::now(); }
        }

        // Trigger: event-driven on a Lazer tick (in-memory, no RPC) when live,
        // else the slow poll fallback. Lazer is the trigger to RE-CHECK, not the
        // liquidatable verdict — see the FIRE tier below.
        let snap: HashMap<u32, f64> = if lazer_on {
            let deadline = Instant::now() + poll;
            loop {
                let cur = arm_feeds().into_iter()
                    .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
                if cur > last_tick_us { last_tick_us = cur; break; }
                if Instant::now() >= deadline { break; }
                std::thread::sleep(Duration::from_millis(tick_poll_ms));
            }
            lazer_snapshot(&lazer_table)
        } else {
            std::thread::sleep(poll);
            lazer_snapshot(&lazer_table)
        };

        // ── FIRE tier: TWO-TIER GATING. Lazer NARROWS the watch-set (below); the
        // ON-CHAIN oracle price GATES the expensive sim/submit work. Only
        // obligations liquidatable at the on-chain price Solend settles against
        // (Solend's authoritative stored health captured fresh at the last rescan —
        // ZERO Lazer projection) earn a sim, ranked by USD deficit and capped
        // top-K so the biggest real opportunity wins. Gating on the Lazer-projected
        // ratio instead flooded ~390 phantoms/cycle through
        // simulateTransaction/Bundle (healthy on-chain), starving a genuine
        // opportunity's sim budget.
        //
        // Solend refreshes obligation health lazily, so some read stored-liquidatable
        // while a fresh sim shows them healthy ("healthy at fresh price"). Once one
        // sim-rejects we SUPPRESS it for the cooldown, so a learned phantom can't
        // keep occupying the capped top-K and crowd out a real opportunity — the
        // fire set converges onto genuine/untested obligations.
        let live_fire: Vec<(Pubkey, f64)> = engine.onchain_liquidatable_ranked().into_iter()
            .filter(|(pk, _)| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown))
            .collect();
        let fire_deferred = live_fire.len().saturating_sub(max_fire);
        let crossed: Vec<Pubkey> = live_fire.into_iter().take(max_fire).map(|(pk, _)| pk).collect();

        // Heartbeat: liveness + detect_lag (the tell this rewrite worked — it
        // must read milliseconds, not the old 30s).
        if lazer_on && hb_every > 0 && last_hb.elapsed() >= Duration::from_secs(hb_every) {
            let total_feeds = arm_feeds().len();
            // Report the tiers DISTINCTLY: `lazer-flagged` is the projected set
            // (leads/diverges — expect hundreds in a moving market); `on-chain
            // liquidatable` is Solend's authoritative stored verdict; `live fire`
            // is that minus the sim-rejected (learned-phantom) cooldown set — the
            // obligations actually eligible to sim this cycle. Only `live fire`
            // (capped) earns sim work. In a calm market `live fire` converges
            // toward 0 as phantoms are learned; if it stays high, real
            // opportunities or a stored-health issue are worth investigating.
            let lazer_near = engine.crossed(&snap, arm_ratio).len();
            let lazer_flagged = engine.crossed(&snap, 1.0).len();
            let onchain_liq = engine.onchain_liquidatable_count();
            let live_fire_ct = engine.onchain_liquidatable_ranked().iter()
                .filter(|(pk, _)| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown)).count();
            let freshest = arm_feeds().into_iter()
                .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            let lag_ms = now_us().saturating_sub(freshest as u128) / 1000;
            let defer = if fire_deferred + arm_deferred > 0 { format!(" | DEFERRED fire {fire_deferred}/arm {arm_deferred}") } else { String::new() };
            eprintln!("[hb] lazer feeds {}/{} live | detect_lag {}ms | watch {} | lazer-flagged {} (≥arm({}) {}) | on-chain liquidatable {} | LIVE fire {} (cap {}) | cache {}{} | {}",
                snap.len(), total_feeds, lag_ms, engine.accounts.len(), lazer_flagged, arm_ratio, lazer_near,
                onchain_liq, live_fire_ct, max_fire, cache.len(), defer, arb_engine::lazer::status(&lazer_table));
            last_hb = Instant::now();
        }

        // ── ARM phase: keep a hot, sim-verified fire tx for the FIRE tier (the
        // on-chain-liquidatable set, top-K) so a tick → sign+send is instant. This
        // is the ONLY place a sim runs, and it is bounded by that small set — the
        // broad Lazer near-threshold set is WATCHED but NEVER simulated (that was
        // the phantom flood). sim_rejected suppresses re-simming an obligation that
        // just sim-rejected ("healthy at fresh price / too small") for a cooldown.
        if lazer_on {
            let fire_keys: HashSet<Pubkey> = crossed.iter().copied().collect();
            cache.retain(|pk, c| fire_keys.contains(pk) && c.built.elapsed() < arm_ttl);
            let candidates: Vec<Pubkey> = crossed.iter().copied()
                .filter(|pk| !cache.contains_key(pk))
                .filter(|pk| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown))
                .collect();
            arm_deferred = candidates.len().saturating_sub(max_arm);
            for pk in candidates.into_iter().take(max_arm) {
                let ratio = engine.onchain_ratio_of(&pk).unwrap_or(0.0);
                match try_arm(&endpoint, &run_dir, &cfg, &crank, &scan, &pk, &repay_fracs, ratio) {
                    Some(c) => { cache.insert(pk, c); }
                    None => { sim_rejected.insert(pk, Instant::now()); }
                }
            }
        }

        // Drop recently-handled obligations (avoid per-tick spin on a standing cross).
        let to_fire: Vec<Pubkey> = crossed.into_iter()
            .filter(|pk| handled.get(pk).is_none_or(|t| t.elapsed() >= handle_cooldown))
            .collect();
        if to_fire.is_empty() { continue; }

        // ── FIRE phase: prefer the armed cache (instant); else arm inline now.
        for pk in &to_fire {
            handled.insert(*pk, Instant::now());
            let ratio = engine.onchain_ratio_of(pk).unwrap_or(1.0);
            let cached = match cache.remove(pk).filter(|c| c.built.elapsed() < arm_ttl) {
                Some(c) => Some(c),
                // Respect the sim cooldown here too: a just-rejected obligation
                // stays on-chain-liquidatable (stored health is fixed until the
                // next rescan), so without this guard the fire path would re-sim
                // the same phantom every cycle — the exact flood we're killing.
                None if sim_rejected.get(pk).is_some_and(|t| t.elapsed() < sim_cooldown) => None,
                None => match try_arm(&endpoint, &run_dir, &cfg, &crank, &scan, pk, &repay_fracs, ratio) {
                    Some(c) => Some(c),
                    None => { sim_rejected.insert(*pk, Instant::now()); None }
                },
            };
            if let Some(c) = cached {
                let armed_from_cache = c.built.elapsed().as_millis() > 0;
                let fire_start = now_us();
                fire_cached(&endpoint, &run_dir, &sender_url, &cfg, &crank, dry_run, pk, &c, fresh_bh,
                    kp.as_ref(), &daily_tip, max_daily_tip_sol, wallet_min_sol, &webhook);
                let done = now_us();
                log_latency(&run_dir, &serde_json::json!({
                    "t": now(), "obligation": pk.to_string(), "protocol": "save", "mode": c.mode.name(),
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
