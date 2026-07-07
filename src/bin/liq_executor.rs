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
//! The tx is profit-or-revert (repay_all fails unless the swap covered the
//! liability), so a losing fire that lands costs only the base fee; the tip ix
//! reverts with it.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [MIN_COLLATERAL_USD=100] [MIN_PROFIT_USD=0.5] [TIP_FRACTION_BPS=3000]
//!        [POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.85] [RUN_DIR=runs]
//!        cargo run --release --bin liq_executor

use arb_engine::jito::send_sender;
use arb_engine::liq_fire::{self, FireCandidate};
use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use arb_engine::marginfi;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
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
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const HEALTHY_ACCOUNT_ERR: u32 = 6068;
/// Largest→smallest: bigger seize = more profit; marginfi rejects over-
/// liquidation (post-liq health must stay ≤ 0), so walk down until one passes.
const SIZE_LADDER: [f64; 5] = [1.0, 0.5, 0.25, 0.1, 0.02];

fn now() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }

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

fn simulate_tx_b64(endpoint: &str, b64tx: &str) -> Option<serde_json::Value> {
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))?;
    sim.get("result")?.get("value").cloned()
}

/// Cheap sim gate: [start_fl, liquidate(asset_amount), end_fl]. Some(true) =
/// marginfi accepts the liquidation at this size.
#[allow(clippy::too_many_arguments)]
fn simulate_gate(
    endpoint: &str, authority: &Pubkey, liquidator_ma: &Pubkey, tp: &Pubkey,
    liquidatee: &Pubkey, acct: &MarginfiAccount, asset_bank: Pubkey, liab_bank: Pubkey,
    asset_amount: u64, oracle_of: &HashMap<Pubkey, Pubkey>,
) -> Option<bool> {
    use solana_message::{v0, VersionedMessage};
    use solana_transaction::versioned::VersionedTransaction;
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
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).ok()?) };
    let res = simulate_tx_b64(endpoint, &b64tx)?;
    let err = &res["err"];
    if err.is_null() { return Some(true); }
    let code = err.get("InstructionError").and_then(|e| e.get(1)).and_then(|c| c.get("Custom")).and_then(|c| c.as_u64());
    match code {
        Some(c) if c as u32 == HEALTHY_ACCOUNT_ERR => Some(false),
        Some(_) => Some(false), // wrong size / other guard — try another rung
        None => None,
    }
}

#[derive(Serialize)]
struct DecisionLog {
    t: u64, liquidatee: String, collateral_usd: f64, ratio: f64,
    seize_native: u64, quoted_usdc_out: f64, est_liab_usdc: f64, est_profit_usdc: f64,
    fire_sim_ok: bool, fired: bool, reason: String,
}
#[derive(Serialize)]
struct TradeLog {
    t: u64, liquidatee: String, seize_native: u64, est_profit_usdc: f64,
    tip_lamports: u64, signature: Option<String>, realized_usdc: Option<f64>, error: Option<String>,
}

struct Scan {
    accts: Vec<(Pubkey, MarginfiAccount)>,
    banks: BankMap,
    oracle_of: HashMap<Pubkey, Pubkey>,
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
    Some(Scan { accts, banks, oracle_of })
}

fn fresh_prices(endpoint: &str, oracle_of: &HashMap<Pubkey, Pubkey>) -> PriceMap {
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let mut by_oracle: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, raw) in &get_multiple(endpoint, &oracle_pks) {
        if let Some(usd) = liq::decode_oracle_price(raw) { by_oracle.insert(*pk, usd); }
    }
    oracle_of.iter().filter_map(|(bk, oc)| Some((*bk, *by_oracle.get(oc)?))).collect()
}

/// Copy-able config bundle for the arm/fire helpers.
#[derive(Clone, Copy)]
struct Cfg {
    liquidator_ma: Pubkey,
    authority: Pubkey,
    tp: Pubkey,
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
    endpoint: &str, run_dir: &str, cfg: &Cfg, scan: &Scan, a: &MarginfiAccount, pk: &Pubkey,
    prices: &PriceMap, mint_tp: &mut HashMap<Pubkey, Pubkey>,
) -> Option<CachedFire> {
    let r = liq::maintenance_health(a, &scan.banks, prices);
    let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).cloned().collect();
    let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).cloned().collect();
    if assets.len() != 1 || liabs.len() != 1 || liabs[0].bank_pk != cfg.usdc_bank { return None; }
    let asset_bank = assets[0].bank_pk;
    let bank = scan.banks.get(&asset_bank)?;
    let native_total = assets[0].asset_shares * bank.asset_share_value;

    // Size by simulation ladder, largest passing fraction first.
    let mut seize = 0u64;
    for frac in SIZE_LADDER {
        let amount = (native_total * frac) as u64;
        if amount == 0 { continue; }
        if simulate_gate(endpoint, &cfg.authority, &cfg.liquidator_ma, &cfg.tp, pk, a, asset_bank, cfg.usdc_bank, amount, &scan.oracle_of) == Some(true) {
            seize = amount;
            break;
        }
    }
    if seize == 0 { return None; } // sim said healthy (emode phantom) — caller cools it down

    let asset_tp = match mint_tp.get(&bank.mint) {
        Some(t) => *t,
        None => { let t = mint_owner(endpoint, &bank.mint)?; mint_tp.insert(bank.mint, t); t }
    };
    let mut obs = Vec::new();
    for b in &a.balances {
        let oc = scan.oracle_of.get(&b.bank_pk)?;
        obs.push(AccountMeta::new_readonly(b.bank_pk, false));
        obs.push(AccountMeta::new_readonly(*oc, false));
    }
    let cand = FireCandidate {
        liquidatee: *pk, asset_bank, asset_mint: bank.mint, asset_token_program: asset_tp,
        asset_amount: seize, liab_bank: cfg.usdc_bank,
        asset_oracle: scan.oracle_of[&asset_bank], liab_oracle: scan.oracle_of[&cfg.usdc_bank],
        liquidatee_obs: obs,
    };
    let price = prices.get(&asset_bank).copied().unwrap_or(0.0);
    let seized_usd = seize as f64 / 10f64.powi(bank.mint_decimals as i32) * price;
    let est_liab = seized_usd * 0.975;
    let sol_usd = scan.banks.iter().find(|(_, b)| b.mint.to_string() == SOL_MINT)
        .and_then(|(bk, _)| prices.get(bk)).copied().unwrap_or(150.0);

    let mut log = DecisionLog {
        t: now(), liquidatee: pk.to_string(), collateral_usd: r.health.weighted_assets, ratio: r.health.ratio(),
        seize_native: seize, quoted_usdc_out: 0.0, est_liab_usdc: est_liab, est_profit_usdc: 0.0,
        fire_sim_ok: false, fired: false, reason: String::new(),
    };
    // Build with a placeholder blockhash (sim replaces it; fire stamps a real one).
    let ph = solana_hash::Hash::default();
    let fire = match liq_fire::build_fire_tx(endpoint, &cand, &cfg.liquidator_ma, &cfg.authority,
        Some(cfg.tip_account), 0, 100_000, cfg.slippage_bps, 20, ph) {
        Ok(f) => f,
        Err(e) => { log.reason = format!("build: {e}"); log_decision(run_dir, &log); return None; }
    };
    log.quoted_usdc_out = fire.quoted_usdc_out as f64 / 1e6;
    let est_profit = fire.quoted_usdc_out as f64 / 1e6 - est_liab;
    log.est_profit_usdc = est_profit;
    let tip_sol = (est_profit * cfg.tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(cfg.min_tip_sol);
    let tip_lamports = (tip_sol * 1e9) as u64;
    if est_profit < cfg.min_profit + tip_sol * sol_usd {
        log.reason = format!("below min profit (est ${est_profit:.2}, tip ${:.2})", tip_sol * sol_usd);
        log_decision(run_dir, &log);
        return None;
    }
    let fire = match liq_fire::build_fire_tx(endpoint, &cand, &cfg.liquidator_ma, &cfg.authority,
        Some(cfg.tip_account), tip_lamports, 100_000, cfg.slippage_bps, 20, ph) {
        Ok(f) => f,
        Err(e) => { log.reason = format!("rebuild: {e}"); log_decision(run_dir, &log); return None; }
    };
    // Ground-truth gate lives HERE (arm time), off the fire critical path.
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
    let sim_ok = simulate_tx_b64(endpoint, &b64tx).map(|res| res["err"].is_null()).unwrap_or(false);
    log.fire_sim_ok = sim_ok;
    if !sim_ok {
        log.reason = "fire-tx sim revert (swap/repay would not cover liability)".into();
        log_decision(run_dir, &log);
        return None;
    }
    Some(CachedFire { tx: fire.tx, tip_lamports, tip_sol, est_profit, seize, built: Instant::now() })
}

/// Fire a cached tx: stamp the fresh blockhash, sign, submit via Sender, log,
/// spawn the realized-P&L readback. The profit-or-revert guard makes this safe
/// without re-simulating — a stale/unprofitable fire reverts for the base fee.
#[allow(clippy::too_many_arguments)]
fn fire_cached(
    endpoint: &str, run_dir: &str, sender_url: &str, cfg: &Cfg, dry_run: bool,
    pk: &Pubkey, cached: &CachedFire, fresh_bh: solana_hash::Hash, kp: Option<&Keypair>,
    daily_tip: &std::sync::Arc<std::sync::Mutex<f64>>, max_daily_tip: f64, wallet_min: f64,
    webhook: &Option<String>,
) {
    let mut log = DecisionLog {
        t: now(), liquidatee: pk.to_string(), collateral_usd: 0.0, ratio: 0.0, seize_native: cached.seize,
        quoted_usdc_out: 0.0, est_liab_usdc: 0.0, est_profit_usdc: cached.est_profit,
        fire_sim_ok: true, fired: false, reason: String::new(),
    };
    println!("★ LIQUIDATABLE  {}  seize {}  est profit ${:.2}  tip {:.5} SOL  (armed {:?} ago)",
        &pk.to_string()[..8], cached.seize, cached.est_profit, cached.tip_sol, cached.built.elapsed());
    if dry_run {
        log.reason = "dry-run: would fire (armed)".into();
        log_decision(run_dir, &log);
        alert(webhook, "liq-dry", &format!("DRY-RUN liquidation: {} est profit ${:.2}", pk, cached.est_profit));
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
    log.fired = true; log.reason = "fired (armed cache)".into();
    log_decision(run_dir, &log);
    let (seize, est_profit, tip_lamports, tip_sol) = (cached.seize, cached.est_profit, cached.tip_lamports, cached.tip_sol);
    match send_sender(sender_url, &tx_b64) {
        Ok(_) => {
            eprintln!("[exec] FIRED {sig}");
            log_trade(run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), realized_usdc: None, error: None });
            let (ep, rd, owner, s, wh) = (endpoint.to_string(), run_dir.to_string(), cfg.authority.to_string(), sig, webhook.clone());
            let tip_counter = daily_tip.clone();
            std::thread::spawn(move || {
                for wait in [5u64, 15, 45] {
                    std::thread::sleep(Duration::from_secs(wait));
                    if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                        *tip_counter.lock().unwrap() += tip_sol;
                        log_trade(&rd, &TradeLog { t: now(), liquidatee: String::new(), seize_native: 0,
                            est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()), realized_usdc: Some(pnl), error: None });
                        alert(&wh, "liq-landed", &format!("liquidation landed {s}: realized ${pnl:.2}"));
                        return;
                    }
                }
                alert(&wh, "liq-miss", &format!("liquidation {s} never confirmed"));
            });
        }
        Err(e) => {
            eprintln!("[exec] send failed: {e}");
            log_trade(run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                est_profit_usdc: est_profit, tip_lamports, signature: None, realized_usdc: None, error: Some(e.to_string()) });
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
    let mut cache: HashMap<Pubkey, CachedFire> = HashMap::new();
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);

    loop {
        // Refresh the watch-set + engine coefficients from a full scan.
        if first || last_scan.elapsed() >= rescan {
            if !first {
                if let Some(s) = full_scan(&endpoint) { scan = s; }
            }
            last_scan = Instant::now();
            let base = fresh_prices(&endpoint, &scan.oracle_of);
            let (prices, _led) = arb_engine::lazer::blend(&scan.banks, &base, &lazer_table, &lazer_map);
            watch = scan.accts.iter().filter_map(|(pk, a)| {
                let r = liq::maintenance_health(a, &scan.banks, &prices);
                (r.missing == 0 && r.health.ratio() >= watch_ratio && r.health.weighted_assets >= min_collateral)
                    .then_some(*pk)
            }).collect();
            // Engine (event-driven trigger): coefficients over the on-chain
            // baseline; Lazer feeds move health between rescans with no RPC.
            let lazer_snapshot: HashMap<u32, f64> = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| Some((f, arb_engine::pyth::get(&lazer_table, f)?.price))).collect();
            let armed = engine.rebuild(&scan.accts, &scan.banks, &base, &mint_feed, &lazer_snapshot, watch_ratio);
            eprintln!("[exec] scan: {} borrowers → watch-set {} (ratio ≥ {}), engine armed {}",
                scan.accts.len(), watch.len(), watch_ratio, armed);
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
                std::thread::sleep(Duration::from_millis(20));
            }
            let snap: HashMap<u32, f64> = arb_engine::lazer::arm_feed_ids().into_iter()
                .filter_map(|f| Some((f, arb_engine::pyth::get(&lazer_table, f)?.price))).collect();
            (engine.crossed(&snap, 1.0), snap)
        } else {
            std::thread::sleep(poll);
            (watch.clone(), HashMap::new())
        };

        // ── ARM phase (lazer mode only): keep a hot, sim-verified fire tx for
        // accounts near the threshold (ratio ≥ arm_ratio) so the cross → send is
        // instant. Prune stale/no-longer-armed entries. Costs Jupiter quotes +
        // sims, but only for the small arm-set, and off the fire critical path.
        if lazer_on {
            let arm_set = engine.crossed(&snap, arm_ratio);
            // Drop cache entries that left the arm-set or went stale.
            cache.retain(|pk, c| arm_set.contains(pk) && c.built.elapsed() < arm_ttl);
            let need: Vec<Pubkey> = arm_set.into_iter()
                .filter(|pk| !cache.contains_key(pk))
                .filter(|pk| sim_rejected.get(pk).is_none_or(|t| t.elapsed() >= sim_cooldown))
                .collect();
            if !need.is_empty() {
                let raw = get_multiple(&endpoint, &need);
                let base = fresh_prices(&endpoint, &scan.oracle_of);
                let (prices, _) = arb_engine::lazer::blend(&scan.banks, &base, &lazer_table, &lazer_map);
                for pk in &need {
                    let Some(a) = raw.get(pk).and_then(|r| MarginfiAccount::decode(r)) else { continue };
                    match try_arm(&endpoint, &run_dir, &cfg, &scan, &a, pk, &prices, &mut mint_tp_cache) {
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
                    match try_arm(&endpoint, &run_dir, &cfg, &scan, &a, pk, &prices, &mut mint_tp_cache) {
                        Some(c) => Some(c),
                        None => { sim_rejected.insert(*pk, Instant::now()); None }
                    }
                }
            };
            if let Some(c) = cached {
                fire_cached(&endpoint, &run_dir, &sender_url, &cfg, dry_run, pk, &c, fresh_bh,
                    kp.as_ref(), &daily_tip_sol, max_daily_tip_sol, wallet_min_sol, &webhook);
            }
        }
    }
}
