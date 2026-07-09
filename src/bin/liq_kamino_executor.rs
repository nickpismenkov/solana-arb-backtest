//! Production Kamino (KLend) liquidation executor — EVENT-DRIVEN, DRY_RUN default.
//!
//! The old build polled stored on-chain health every 30s (RESCAN_SECS) / re-read
//! the watch-set every 5s (POLL_MS). That loses the race: Kamino's Scope oracle
//! updates on-chain and whoever submits a liquidate in the same/next slot wins,
//! while a 5–30s poll shows up long after. This rewrite mirrors the marginfi/Save
//! executors — a Lazer WebSocket feeds an in-memory health engine
//! (src/kamino_engine.rs) that recomputes every obligation's bf_debt/threshold on
//! each ~ms price tick with ZERO RPC, so a cross is noticed in ms not seconds.
//!
//! TWO-TIER gating (the overflag fix): Lazer NARROWS the set; the ON-CHAIN Scope
//! price GATES the expensive work. KLend liquidations settle at the on-chain Scope
//! oracle, and Lazer LEADS/diverges from Scope, so the Lazer-projected
//! "liquidatable" set is ~900 phantoms that are healthy on-chain. Building a
//! quote+sim fire tx for each hammered Jupiter into a 429 storm and starved real
//! opportunities. So:
//!   full scan (RESCAN_SECS): v1 (1 deposit / 1 wired-debt borrow, non-elevation)
//!     obligations + their reserves → kamino_engine watch-set (stored on-chain
//!     health + per-side Lazer anchors)
//!   ARM tier (cheap, Lazer): the near-threshold watch-set — recomputed per tick
//!     with ZERO RPC, NO Jupiter, NO sim. Only narrows who's worth watching.
//!   FIRE tier (expensive): ONLY obligations liquidatable at the on-chain Scope
//!     price (engine.on_chain_liquidatable_ranked — stored health, not the Lazer
//!     projection), ranked by USD deficit, capped to MAX_FIRE_PER_CYCLE. These
//!     get the Jupiter quote + sim + submit; a quote/sim reject → cooldown so the
//!     same candidate isn't re-hammered every cycle.
//!
//! Kamino prices via Scope (its own oracle) which we cannot crank ourselves, so
//! unlike Save there is no crank/bundle mode — a single Sender tx. Safety is
//! profit-or-revert: the JupLend fixed-amount payback fails unless the
//! seized-collateral swap covered the flash-borrow, so a premature or losing fire
//! that lands costs only the base fee; the fire sim is a clean full-execution OR a
//! revert only at Kamino's own liquidate/health gate.
//!
//! v1.5 debt scope (preserved from PR #67): any debt with a wired JupLend flash
//! market — USDC / USDT / wSOL.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [PYTH_LAZER_TOKEN=… (required for event-driven)] [MIN_DEBT_USD=100]
//!        [MIN_PROFIT_USD=0.5] [CLOSE_FACTOR=0.2] [MAX_BORROW_USD=5000]
//!        [WATCH_RATIO=0.9] [ARM_RATIO=0.97] [RATIO_CAP=3] [RESCAN_SECS=30]
//!        [TICK_POLL_MS=1] [POLL_MS=5000] [MAX_FIRE_PER_CYCLE=4]
//!        [SIM_COOLDOWN_SECS=60] [HANDLE_COOLDOWN_SECS=20] [JUP_API_BASE=…]
//!        [SLIPPAGE_BPS=100] [MAX_SWAP_ACCOUNTS=20]
//!        cargo run --release --bin liq_kamino_executor

use arb_engine::jito::send_sender;
use arb_engine::kamino::{Obligation, Reserve};
use arb_engine::kamino_engine::Engine;
use arb_engine::kamino_fire::{self, KaminoFireCandidate};
use arb_engine::kamino_ix::ReserveAccounts;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use serde::Serialize;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const OBLIGATION_SIZE: usize = 3344;
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
// [cu, cu_price, ata, ata, ata, borrow, refresh, refresh, refresh_ob, LIQUIDATE, …]
const LIQUIDATE_IX_INDEX: u64 = 9;

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
fn b64tx(tx: &VersionedTransaction) -> String {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx).unwrap())
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
fn mint_owner(endpoint: &str, mint: &Pubkey, cache: &mut HashMap<Pubkey, Pubkey>) -> Pubkey {
    if let Some(p) = cache.get(mint) { return *p; }
    let p = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[mint.to_string(),{"encoding":"base64"}]}))
        .and_then(|v| v["result"]["value"]["owner"].as_str().map(String::from))
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| Pubkey::from_str(TOKEN_PROGRAM).unwrap());
    cache.insert(*mint, p);
    p
}
fn latest_blockhash(endpoint: &str) -> Option<solana_hash::Hash> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))?;
    solana_hash::Hash::from_str(v["result"]["value"]["blockhash"].as_str()?).ok()
}
fn sol_balance(endpoint: &str, owner: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[owner]}))
        .and_then(|v| v["result"]["value"].as_u64()).map(|l| l as f64 / 1e9).unwrap_or(0.0)
}

/// Full-tx sim outcome, classified by where it stopped (mirrors kamino_fire_probe).
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
enum SimClass {
    /// err null — whole flashloan-wrapped tx executes (on-chain liquidatable + profitable).
    Clean,
    /// Reverts only at Kamino's own liquidate/health/close-factor gate (ix 9) —
    /// wiring OK, armed AHEAD of the on-chain cross.
    LiquidateGate,
    /// Reverts at some other ix — a wiring problem; don't arm.
    OtherRevert(u64),
    /// RPC rejected the sim (no value) — treat as unusable.
    Reject,
}
fn sim_class(endpoint: &str, b64: &str) -> SimClass {
    let Some(sim) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]})) else {
        return SimClass::Reject;
    };
    let Some(res) = sim.get("result").and_then(|r| r.get("value")) else { return SimClass::Reject; };
    if res["err"].is_null() { return SimClass::Clean; }
    match res["err"].get("InstructionError").and_then(|e| e.get(0)).and_then(|i| i.as_u64()) {
        Some(LIQUIDATE_IX_INDEX) => SimClass::LiquidateGate,
        Some(i) => SimClass::OtherRevert(i),
        None => SimClass::Reject,
    }
}

#[derive(Serialize)]
struct DecisionLog {
    t: u64, obligation: String, protocol: &'static str, ratio: f64, debt_usd: f64,
    repay_usd: f64, quoted_usdc_out: f64, est_profit_usdc: f64, fire_sim_ok: bool, fired: bool, reason: String,
}
#[derive(Serialize)]
struct TradeLog {
    t: u64, obligation: String, protocol: &'static str, repay_usd: f64, est_profit_usdc: f64,
    tip_lamports: u64, signature: Option<String>, realized_usdc: Option<f64>, error: Option<String>,
}

/// A full scan: v1 obligations + the reserve → Lazer-feed map they resolve to.
/// (Fresh reserve prices/wiring are re-fetched at arm time; only the stable
/// reserve→feed mapping is kept here for the engine's ratio anchoring.)
struct KaminoScan {
    obls: Vec<(Pubkey, Obligation)>,
    ob_index: HashMap<Pubkey, Obligation>,
    reserve_feed: HashMap<Pubkey, u32>, // reserve pk → Lazer feed id
    reserve_mint: HashMap<Pubkey, Pubkey>, // reserve pk → liquidity mint (for the wired-flash-market gate)
}

fn scan_obligations(endpoint: &str) -> Vec<(Pubkey, Obligation)> {
    let resp = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize":OBLIGATION_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}}]}]}));
    resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default().iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, Obligation::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, o): &(Pubkey, Obligation)| o.deposits.len() == 1 && o.borrows.len() == 1 && o.elevation_group == 0).collect()
}

/// Scan obligations, keep v1 shape, load their reserves (price + wiring), and
/// build the reserve → Lazer-feed map (via each reserve's liquidity mint).
fn full_scan_kamino(
    endpoint: &str, min_debt: f64, mint_feed: &HashMap<Pubkey, u32>,
) -> Option<KaminoScan> {
    let obls = scan_obligations(endpoint);
    if obls.is_empty() { return None; }
    let reserve_pks: Vec<Pubkey> = obls.iter()
        .flat_map(|(_, o)| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
        .collect::<HashSet<_>>().into_iter().collect();
    let raw = get_multiple(endpoint, &reserve_pks);
    let mut reserve_feed = HashMap::new();
    let mut reserve_mint = HashMap::new();
    let mut reserves: HashMap<Pubkey, Reserve> = HashMap::new();
    for pk in &reserve_pks {
        let Some(d) = raw.get(pk) else { continue };
        // The liquidity mint drives both the Lazer feed (ratio anchor) and the
        // wired-flash-market gate.
        if let Some(ra) = ReserveAccounts::decode(*pk, d) {
            reserve_mint.insert(*pk, ra.liquidity_mint);
            if let Some(f) = mint_feed.get(&ra.liquidity_mint) { reserve_feed.insert(*pk, *f); }
        }
        // The reserve's cached Scope price + config → recompute CURRENT health.
        if let Some(r) = Reserve::decode(d) { reserves.insert(*pk, r); }
    }

    // Anchor on CURRENT on-chain (Scope) health, NOT the obligation's stored health.
    // The stored bf_adjusted_debt/unhealthy_borrow_value are only as fresh as the
    // obligation's last refresh — a position that WAS underwater but has since been
    // priced healthy still reads "liquidatable" from its stale stored values, which
    // over-flagged the fire tier (DRY_RUN: ~12 phantoms, all reverting at the
    // liquidate gate). recompute() reprices every position from the reserves' fresh
    // Scope prices (verified in kamino.rs), so the engine anchors on what KLend will
    // actually settle at. Interest isn't re-accrued (conservative → no false-positive).
    let obls: Vec<(Pubkey, Obligation)> = obls.into_iter().filter_map(|(pk, mut o)| {
        let rc = arb_engine::kamino::recompute(&o, &reserves);
        if rc.trustworthy() {
            o.bf_adjusted_debt = rc.bf_adjusted_debt;
            o.unhealthy_borrow_value = rc.unhealthy_borrow_value;
            o.allowed_borrow_value = rc.allowed_borrow_value;
            o.deposited_value = rc.deposited_value;
            o.borrowed_value = o.borrows.iter().filter_map(|(res, bamt)| {
                let r = reserves.get(res)?;
                Some((bamt / 10f64.powi(r.mint_decimals as i32)) * r.market_price)
            }).sum();
        }
        (o.borrowed_value >= min_debt).then_some((pk, o))
    }).collect();
    let ob_index = obls.iter().cloned().collect();
    Some(KaminoScan { obls, ob_index, reserve_feed, reserve_mint })
}

#[derive(Clone, Copy)]
struct Cfg {
    authority: Pubkey,
    tip_account: Pubkey,
    tip_fraction_bps: u64,
    min_tip_sol: f64,
    min_profit: f64,
    close_factor: f64,
    max_borrow_usd: f64,
    slippage_bps: u32,
    max_swap_accounts: usize,
}

/// A sim-verified fire tx kept hot for an armed obligation. Compiled with a
/// placeholder blockhash (sim replaces it); the real hash is stamped at fire.
#[derive(Clone)]
struct CachedFire {
    tx: VersionedTransaction,
    tip_lamports: u64,
    tip_sol: f64,
    est_profit: f64,
    repay_usd: f64,
    debt_usd: f64,
    ratio: f64,
    /// true = sim ran fully CLEAN (already liquidatable on-chain); false = armed
    /// ahead of the on-chain cross (sim reverted only at the liquidate gate).
    clean: bool,
    built: Instant,
}

/// Build + size + profit-gate + sim-gate one obligation → CachedFire. This is the
/// only place a fire tx is built (build + Jupiter quote + sim), all off the fire
/// critical path. Accepts a sim that is CLEAN or reverts only at Kamino's own
/// liquidate gate (armed ahead of the Scope cross).
#[allow(clippy::too_many_arguments)]
fn try_arm(
    endpoint: &str, run_dir: &str, cfg: &Cfg, scan: &KaminoScan,
    pk: &Pubkey, engine_ratio: f64, tp_cache: &mut HashMap<Pubkey, Pubkey>,
) -> Option<CachedFire> {
    let market = Pubkey::from_str(MAIN_MARKET).unwrap();
    let o = scan.ob_index.get(pk)?;
    if o.deposits.len() != 1 || o.borrows.len() != 1 || o.elevation_group != 0 { return None; }
    let withdraw_pk = o.deposits[0].0;
    let repay_pk = o.borrows[0].0;

    let mut log = DecisionLog {
        t: now(), obligation: pk.to_string(), protocol: "kamino", ratio: engine_ratio,
        debt_usd: 0.0, repay_usd: 0.0, quoted_usdc_out: 0.0, est_profit_usdc: 0.0,
        fire_sim_ok: false, fired: false, reason: String::new(),
    };
    let skip = |log: &mut DecisionLog, reason: &str| { log.reason = reason.into(); log_decision(run_dir, log); };

    // Fresh reserve data (prices move; obligation reserves are stable).
    let raw = get_multiple(endpoint, &[withdraw_pk, repay_pk]);
    let (Some(wr_data), Some(rr_data)) = (raw.get(&withdraw_pk), raw.get(&repay_pk)) else {
        skip(&mut log, "reserve fetch failed"); return None;
    };
    let (Some(wr), Some(rr)) = (ReserveAccounts::decode(withdraw_pk, wr_data), ReserveAccounts::decode(repay_pk, rr_data)) else {
        skip(&mut log, "reserve accounts decode failed"); return None;
    };
    let (Some(wr_res), Some(rr_res)) = (Reserve::decode(wr_data), Reserve::decode(rr_data)) else {
        skip(&mut log, "reserve decode failed"); return None;
    };
    // v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL). Preserved.
    if !arb_engine::flashloan::has_market(&rr.liquidity_mint) { skip(&mut log, "debt mint has no wired flash market"); return None; }

    let debt_dec = rr_res.mint_decimals as i32;
    let debt_price = rr_res.market_price.max(1e-9);
    let debt_usd = (o.borrows[0].1 / 10f64.powi(debt_dec)) * rr_res.market_price;
    let repay_usd = (debt_usd * cfg.close_factor).min(cfg.max_borrow_usd).max(1.0);
    let repay_amount = (repay_usd / debt_price * 10f64.powi(debt_dec)) as u64;
    let bonus = 1.05;
    let seized_native = repay_usd * bonus / wr_res.market_price.max(1e-9) * 10f64.powi(wr_res.mint_decimals as i32);
    let swap_in_amount = (seized_native * 0.995) as u64;
    log.debt_usd = debt_usd;
    log.repay_usd = repay_usd;

    let cand = KaminoFireCandidate {
        obligation: *pk, lending_market: market, repay_reserve: rr.clone(), withdraw_reserve: wr.clone(),
        obligation_reserves: vec![withdraw_pk, repay_pk],
        withdraw_liquidity_mint: wr.liquidity_mint,
        withdraw_liquidity_token_program: mint_owner(endpoint, &wr.liquidity_mint, tp_cache),
        withdraw_collateral_token_program: mint_owner(endpoint, &wr.collateral_mint, tp_cache),
        repay_liquidity_token_program: mint_owner(endpoint, &rr.liquidity_mint, tp_cache),
        repay_amount, swap_in_amount,
    };
    // Placeholder blockhash — sim replaces it; the live fire stamps the fresh hash.
    let ph = solana_hash::Hash::default();

    // First build (no tip) to get the Jupiter quote for the profit gate.
    let fire = match kamino_fire::build_fire_tx(endpoint, &cand, &cfg.authority, None, 0, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, ph) {
        Ok(f) => f,
        Err(e) => { skip(&mut log, &format!("build: {e}")); return None; }
    };
    let quoted_usd = fire.quoted_usdc_out as f64 / 10f64.powi(debt_dec) * debt_price;
    let est_profit = quoted_usd - repay_usd;
    log.quoted_usdc_out = quoted_usd;
    log.est_profit_usdc = est_profit;
    let sol_usd = 150.0; // conservative; tip is tiny vs profit
    let tip_sol = (est_profit * cfg.tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(cfg.min_tip_sol);
    let tip_lamports = (tip_sol * 1e9) as u64;
    if est_profit < cfg.min_profit + tip_sol * sol_usd {
        skip(&mut log, &format!("below min profit (est ${est_profit:.2}, tip ${:.2})", tip_sol * sol_usd));
        return None;
    }

    // Final build WITH the tip, sim-gate.
    let fire = match kamino_fire::build_fire_tx(endpoint, &cand, &cfg.authority, Some(cfg.tip_account), tip_lamports, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, ph) {
        Ok(f) => f,
        Err(e) => { skip(&mut log, &format!("rebuild: {e}")); return None; }
    };
    let class = sim_class(endpoint, &b64tx(&fire.tx));
    let clean = class == SimClass::Clean;
    log.fire_sim_ok = matches!(class, SimClass::Clean | SimClass::LiquidateGate);
    match class {
        SimClass::Clean | SimClass::LiquidateGate => {}
        SimClass::OtherRevert(i) => { skip(&mut log, &format!("sim revert at ix {i} (wiring) — not arming")); return None; }
        SimClass::Reject => { skip(&mut log, "sim rejected by RPC"); return None; }
    }
    log.reason = if clean { "armed (clean — liquidatable on-chain now)".into() } else { "armed (ahead — reverts at liquidate gate until Scope crosses)".into() };
    log_decision(run_dir, &log);
    Some(CachedFire {
        tx: fire.tx, tip_lamports, tip_sol, est_profit, repay_usd, debt_usd, ratio: engine_ratio, clean,
        built: Instant::now(),
    })
}

/// Fire a cached tx: stamp fresh blockhash, sign, submit via Helius Sender, log,
/// spawn P&L readback. No build/quote/sim here — the hot path is submit-only.
#[allow(clippy::too_many_arguments)]
fn fire_cached(
    endpoint: &str, run_dir: &str, sender_url: &str, cfg: &Cfg, dry_run: bool,
    pk: &Pubkey, cached: &CachedFire, fresh_bh: solana_hash::Hash, kp: Option<&Keypair>,
    daily_tip: &std::sync::Arc<std::sync::Mutex<f64>>, max_daily_tip: f64, wallet_min: f64,
    webhook: &Option<String>,
) {
    let mut log = DecisionLog {
        t: now(), obligation: pk.to_string(), protocol: "kamino", ratio: cached.ratio, debt_usd: cached.debt_usd,
        repay_usd: cached.repay_usd, quoted_usdc_out: 0.0, est_profit_usdc: cached.est_profit,
        fire_sim_ok: true, fired: false, reason: String::new(),
    };
    println!("★ KAMINO LIQUIDATABLE {}  debt ${:.0}  repay ${:.2}  est profit ${:.2}  tip {:.5} SOL  ({} armed {:?} ago)",
        &pk.to_string()[..8], cached.debt_usd, cached.repay_usd, cached.est_profit, cached.tip_sol,
        if cached.clean { "clean" } else { "ahead" }, cached.built.elapsed());
    if dry_run {
        log.reason = format!("dry-run: would fire (armed, {})", if cached.clean { "clean" } else { "ahead" });
        log_decision(run_dir, &log);
        alert(webhook, "kliq-dry", &format!("DRY-RUN Kamino liq {} est profit ${:.2}", pk, cached.est_profit));
        return;
    }
    if *daily_tip.lock().unwrap() + cached.tip_sol > max_daily_tip {
        log.reason = "daily tip cap".into(); log_decision(run_dir, &log);
        alert(webhook, "kliq-cap", "daily tip cap reached"); return;
    }
    if sol_balance(endpoint, &cfg.authority.to_string()) < wallet_min {
        log.reason = "wallet below floor".into(); log_decision(run_dir, &log);
        alert(webhook, "kliq-floor", "wallet below floor — not firing"); return;
    }
    let mut tx = cached.tx.clone();
    tx.message.set_recent_blockhash(fresh_bh);
    let kp = kp.unwrap();
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    let tx_b64 = b64tx(&tx);
    let (repay_usd, est_profit, tip_lamports, tip_sol) = (cached.repay_usd, cached.est_profit, cached.tip_lamports, cached.tip_sol);
    log.fired = true; log.reason = "fired (armed cache)".into();
    log_decision(run_dir, &log);
    match send_sender(sender_url, &tx_b64) {
        Ok(_) => {
            eprintln!("[kexec] FIRED {sig}");
            log_trade(run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "kamino",
                repay_usd, est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), realized_usdc: None, error: None });
            let (ep, rd, owner, s, wh, tc) =
                (endpoint.to_string(), run_dir.to_string(), cfg.authority.to_string(), sig, webhook.clone(), daily_tip.clone());
            std::thread::spawn(move || {
                for wait in [5u64, 15, 45] {
                    std::thread::sleep(Duration::from_secs(wait));
                    if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                        *tc.lock().unwrap() += tip_sol;
                        log_trade(&rd, &TradeLog { t: now(), obligation: String::new(), protocol: "kamino",
                            repay_usd: 0.0, est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()), realized_usdc: Some(pnl), error: None });
                        alert(&wh, "kliq-landed", &format!("Kamino liq landed {s}: realized ${pnl:.2}"));
                        return;
                    }
                }
                alert(&wh, "kliq-miss", &format!("Kamino liq {s} never confirmed"));
            });
        }
        Err(e) => {
            eprintln!("[kexec] send failed: {e}");
            log_trade(run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "kamino",
                repay_usd, est_profit_usdc: est_profit, tip_lamports, signature: None, realized_usdc: None, error: Some(e.to_string()) });
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
    let close_factor: f64 = std::env::var("CLOSE_FACTOR").ok().and_then(|s| s.parse().ok()).unwrap_or(0.2);
    let max_borrow_usd: f64 = std::env::var("MAX_BORROW_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(5000.0);
    let rescan = Duration::from_secs(std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30));
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.9);
    let arm_ratio: f64 = std::env::var("ARM_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.97);
    let max_fire: usize = std::env::var("MAX_FIRE_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(4);
    let tick_poll_ms: u64 = std::env::var("TICK_POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1);
    let poll = Duration::from_millis(std::env::var("POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(5000));
    let sim_cooldown = Duration::from_secs(std::env::var("SIM_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(60));
    let handle_cooldown = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let hb_every = std::env::var("HEARTBEAT_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30u64);
    let tip_fraction_bps: u64 = std::env::var("TIP_FRACTION_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let min_tip_sol: f64 = std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let slippage_bps: u32 = std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100);
    let max_swap_accounts: usize = std::env::var("MAX_SWAP_ACCOUNTS").ok().and_then(|s| s.parse().ok()).unwrap_or(20);
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let tip_account = Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT")
        .unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap();
    let webhook = std::env::var("ALERT_WEBHOOK").ok();

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap());

    let cfg = Cfg {
        authority, tip_account, tip_fraction_bps, min_tip_sol, min_profit, close_factor,
        max_borrow_usd, slippage_bps, max_swap_accounts,
    };

    // Lazer WebSocket: the event-driven trigger. Without a token the loop still
    // runs but only on the slow poll fallback — warn loudly, since that's the
    // exact poll regression this rewrite exists to kill.
    let lazer_table = arb_engine::pyth::new_table();
    let mint_feed = arb_engine::lazer::mint_feed_map();
    let lazer_on = std::env::var("PYTH_LAZER_TOKEN").ok().filter(|t| !t.is_empty()).map(|token| {
        arb_engine::lazer::spawn_lazer_thread(token, arb_engine::lazer::arm_feed_ids(), lazer_table.clone());
        eprintln!("[kexec] Pyth Lazer event-driven trigger ENABLED");
    }).is_some();
    if !lazer_on { eprintln!("[kexec] WARNING: no PYTH_LAZER_TOKEN — falling back to slow poll (the regression). Set the token for ms detection."); }

    eprintln!("[kexec] Kamino liquidation executor {}  authority={}  min_debt=${min_debt} min_profit=${min_profit} rescan={:?} tick_poll={}ms lazer={}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, rescan, tick_poll_ms, lazer_on);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[kexec] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor {wallet_min_sol}");
    }

    let mut engine = Engine::new(min_debt, ratio_cap);
    let mut scan = full_scan_kamino(&endpoint, min_debt, &mint_feed).expect("initial scan");
    let mut last_scan = Instant::now();
    let mut tp_cache: HashMap<Pubkey, Pubkey> = HashMap::new();

    let daily_tip = std::sync::Arc::new(std::sync::Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);
    let mut handled: HashMap<Pubkey, Instant> = HashMap::new();
    // Quote/sim-rejected cooldown: once a candidate is quoted+sim'd and rejected
    // (healthy at the fresh price, unprofitable, or a Jupiter 429), don't re-quote
    // it for `sim_cooldown` — stops re-hammering the same phantoms every cycle.
    let mut sim_rejected: HashMap<Pubkey, Instant> = HashMap::new();
    let mut last_tick_us: u64 = 0;
    let mut last_hb = Instant::now() - Duration::from_secs(9999);
    let mut fire_deferred = 0usize;
    // Debt mints seen in the watch-set with no wired flash market — logged once
    // (a one-time summary), never per-cycle.
    let mut logged_unwired: HashSet<Pubkey> = HashSet::new();
    let mut first = true;

    let lazer_snapshot = |table: &arb_engine::pyth::PriceTable| -> HashMap<u32, f64> {
        arb_engine::lazer::arm_feed_ids().into_iter()
            .filter_map(|f| Some((f, arb_engine::pyth::get(table, f)?.price))).collect()
    };

    loop {
        // Rebuild the watch-set + engine from a full scan.
        if first || last_scan.elapsed() >= rescan {
            if !first {
                if let Some(s) = full_scan_kamino(&endpoint, min_debt, &mint_feed) { scan = s; }
            }
            last_scan = Instant::now();
            let snap = lazer_snapshot(&lazer_table);
            let armed = engine.rebuild(&scan.obls, &scan.reserve_feed, watch_ratio, &snap);
            eprintln!("[kexec] scan: {} v1 obligations (≥ ${min_debt}) → engine watch-set {} (ratio ≥ {})",
                scan.obls.len(), armed, watch_ratio);
            // One-time summary of watch-set debts with no wired flash market — these
            // can never fire, so they're excluded from fire candidates (never a build
            // attempt). Log the mint once, not per-cycle.
            let mut unwired_now = 0usize;
            for w in &engine.accounts {
                let Some(mint) = scan.reserve_mint.get(&w.debt_reserve) else { continue };
                if arb_engine::flashloan::has_market(mint) { continue; }
                unwired_now += 1;
                if logged_unwired.insert(*mint) {
                    eprintln!("[kexec] unwired debt mint (no JupLend flash market) — will skip: {mint}");
                }
            }
            if unwired_now > 0 {
                eprintln!("[kexec] {unwired_now}/{} watch-set obligations have an unwired debt mint (excluded from fire candidates)", engine.accounts.len());
            }
            first = false;
        }

        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip.lock().unwrap() = 0.0; }
        if last_bh.elapsed() >= Duration::from_secs(2) {
            if let Some(bh) = latest_blockhash(&endpoint) { fresh_bh = bh; last_bh = Instant::now(); }
        }

        // Trigger cadence: wake on a Lazer tick (in-memory, no RPC) when live, else
        // the slow poll fallback. The tick only paces the loop — it NARROWS which
        // obligations are near threshold (the watch-set), but does NOT decide who
        // fires. The fire set is gated on the ON-CHAIN Scope price below, because
        // Lazer LEADS/diverges from Scope and its projected "liquidatable" set is
        // ~900 phantoms that are healthy on-chain.
        let snap: HashMap<u32, f64> = if lazer_on {
            let deadline = Instant::now() + poll;
            loop {
                let cur = arb_engine::lazer::arm_feed_ids().into_iter()
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

        // Heartbeat: liveness + detect_lag (the tell this rewrite worked — it must
        // read milliseconds, not the old 5–30s poll interval).
        if lazer_on && hb_every > 0 && last_hb.elapsed() >= Duration::from_secs(hb_every) {
            let total_feeds = arb_engine::lazer::arm_feed_ids().len();
            let near = engine.crossed(&snap, arm_ratio).len();
            // TWO distinct counts: lazer-flagged (the projected set — cheap ARM tier,
            // no Jupiter) vs on-chain liquidatable (the real FIRE candidates at the
            // Scope price). In a calm market on-chain M should be single-digit/zero
            // even while lazer-flagged L is hundreds — that gap IS the phantom set.
            let lazer_flagged = engine.crossed(&snap, 1.0).len();
            let on_chain = engine.on_chain_liquidatable_count();
            let freshest = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            let lag_ms = now_us().saturating_sub(freshest as u128) / 1000;
            let defer = if fire_deferred > 0 { format!(" | DEFERRED fire {fire_deferred}/cycle") } else { String::new() };
            eprintln!("[hb] lazer feeds {}/{} live | detect_lag {}ms | watch {} | {} within arm({}) | lazer-flagged {} | on-chain liquidatable {} | fire-cap {}{} | {}",
                snap.len(), total_feeds, lag_ms, engine.accounts.len(), near, arm_ratio, lazer_flagged, on_chain, max_fire, defer,
                arb_engine::lazer::status(&lazer_table));
            last_hb = Instant::now();
        }

        // ── ARM tier (cheap, Lazer-driven): the near-threshold watch-set is
        // maintained by engine.rebuild — no Jupiter, no sim. It only NARROWS the
        // universe. Nothing to do here per tick; it's reported in the heartbeat.

        // ── FIRE tier (expensive): ONLY obligations liquidatable at the ON-CHAIN
        // Scope price (health RECOMPUTED from fresh reserve prices at the last
        // rescan — NOT the Lazer projection). Ranked by USD deficit, capped to top-K/cycle so the biggest
        // REAL opportunity wins a bounded quote/sim budget. This is the ONLY place
        // Jupiter is called — the whole 429-storm fix is that this set is ~0 in a
        // calm market instead of the ~900 the Lazer projection used to feed here.
        let fire_ranked = engine.on_chain_liquidatable_ranked();
        let is_wired = |pk: &Pubkey| -> bool {
            engine.reserves_of(pk)
                .and_then(|(_, debt)| scan.reserve_mint.get(&debt))
                .map(arb_engine::flashloan::has_market)
                .unwrap_or(false)
        };
        let fire_candidates: Vec<Pubkey> = fire_ranked.into_iter()
            .map(|(pk, _)| pk)
            .filter(is_wired)                                                            // unwired debt → can never fire; drop cleanly
            .filter(|pk| handled.get(pk).is_none_or(|t| t.elapsed() >= handle_cooldown)) // not just handled (standing cross)
            .filter(|pk| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown)) // not in quote/sim-reject cooldown
            .collect();
        fire_deferred = fire_candidates.len().saturating_sub(max_fire);
        for pk in fire_candidates.into_iter().take(max_fire) {
            handled.insert(pk, Instant::now());
            let ratio = engine.accounts.iter().find(|w| w.obligation == pk).map(|w| w.on_chain_ratio()).unwrap_or(1.0);
            // Build + Jupiter quote (jup.rs backoff, honors JUP_API_BASE) + sim gate
            // (the authoritative on-chain liquidatability/profit check).
            let fire_start = now_us();
            match try_arm(&endpoint, &run_dir, &cfg, &scan, &pk, ratio, &mut tp_cache) {
                Some(c) => {
                    fire_cached(&endpoint, &run_dir, &sender_url, &cfg, dry_run, &pk, &c, fresh_bh,
                        kp.as_ref(), &daily_tip, max_daily_tip_sol, wallet_min_sol, &webhook);
                    let done = now_us();
                    // Only meaningful with a real Lazer tick (appeared_us = its publish ts).
                    if lazer_on { log_latency(&run_dir, &serde_json::json!({
                        "t": now(), "obligation": pk.to_string(), "protocol": "kamino",
                        "clean": c.clean, "appeared_us": last_tick_us,
                        "detected_lag_ms": fire_start.saturating_sub(last_tick_us as u128) / 1000,
                        "submit_lag_ms": done.saturating_sub(last_tick_us as u128) / 1000,
                        "fire_submit_ms": done.saturating_sub(fire_start) / 1000,
                        "armed": false, "dry_run": dry_run,
                    })); }
                }
                // Quote/sim rejected (healthy at fresh price, unprofitable, or 429) →
                // cooldown so we don't re-hammer the same candidate next cycle.
                None => { sim_rejected.insert(pk, Instant::now()); }
            }
        }
    }
}
