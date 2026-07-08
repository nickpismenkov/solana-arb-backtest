//! Production Save (Solend) liquidation executor — continuous loop, DRY_RUN
//! default. Simpler than the marginfi executor: Save computes health on-chain
//! (borrowed_value vs unhealthy_borrow_value, refreshed by Solend's crankers),
//! so the stored obligation state IS the trigger — no emode math, no engine.
//!
//! Pipeline per cycle:
//!   scan obligations (dataSlice header) → v1 (1 collateral / 1 USDC debt),
//!     liquidatable, ≥ MIN_DEBT_USD → candidates, ranked by debt
//!   for the top MAX_FIRE_PER_CYCLE:
//!     size the repay by simulation ladder (largest passing fraction)
//!     → build the atomic fire tx (flash-borrow → liquidate+redeem → swap → repay)
//!     → profit gate (quoted USDC out vs repay + tip)
//!     → full fire-tx simulation (ground truth)
//!     → DRY_RUN: log · LIVE: sign + Sender submit, readback P&L
//!
//! Profit-or-revert (payback_all fails unless the swap covered the borrow), so a
//! losing fire that lands costs only the base fee; the tip reverts with it.
//!
//! Shared-wallet risk budget: when running alongside liq_executor, split the
//! daily tip budget — set MAX_DAILY_TIP_SOL on each to (total / N). The floor
//! and profit gates are enforced per-process.
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [REPAY_FRACS=0.2,0.1,0.05]
//!        [MAX_FIRE_PER_CYCLE=4] [RESCAN_SECS=20] [SLIPPAGE_BPS=100]
//!        [MAX_SWAP_ACCOUNTS=18] cargo run --release --bin liq_save_executor

use arb_engine::jito::send_sender;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use arb_engine::save::{self, Obligation, Reserve};
use arb_engine::save_fire::{build_save_fire_tx, SaveFireCandidate};
use serde::Serialize;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

fn now() -> u64 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() }

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
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
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
fn simulate_ok(endpoint: &str, b64tx: &str) -> bool {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))
        .and_then(|v| v["result"].get("value").map(|val| val["err"].is_null())).unwrap_or(false)
}

#[derive(Serialize)]
struct DecisionLog {
    t: u64, obligation: String, protocol: &'static str, debt_usd: f64, ratio: f64,
    repay_native: u64, quoted_usdc_out: f64, est_profit_usdc: f64, fired: bool, reason: String,
}
#[derive(Serialize)]
struct TradeLog {
    t: u64, obligation: String, protocol: &'static str, repay_native: u64, est_profit_usdc: f64,
    tip_lamports: u64, signature: Option<String>, realized_usdc: Option<f64>, error: Option<String>,
}

/// A liquidatable v1 candidate found by the dataSlice scan.
struct Candidate {
    obligation: Pubkey,
    collateral_reserve: Pubkey,
    debt_usd: f64,
    ratio: f64,
}

/// Scan obligations (dataSlice header only) → v1-USDC-debt, liquidatable,
/// ≥ min_debt, ratio ≤ ratio_cap (absurd ratios = mis-priced collateral valued
/// near zero, not real opportunities — they'd only waste sim slots). Ranked by
/// debt desc. Header offsets from save.rs / Solend Pack.
fn scan_candidates(endpoint: &str, usdc_reserve: &Pubkey, min_debt: f64, ratio_cap: f64) -> Vec<Candidate> {
    // Slice covers header values + lens + deposit[0].reserve + borrow[0].reserve.
    let resp = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[save::SOLEND_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":324},
            "filters":[{"dataSize":1300},{"memcmp":{"offset":10,"bytes":save::MAIN_POOL}}]}]}));
    let entries = resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default();
    let wad = |d: &[u8], o: usize| u128::from_le_bytes(d[o..o + 16].try_into().unwrap()) as f64 / 1e18;
    let pk = |d: &[u8], o: usize| Pubkey::new_from_array(d[o..o + 32].try_into().unwrap());
    let mut out = Vec::new();
    for e in &entries {
        let Some(obl) = e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()) else { continue };
        let Some(d) = b64(&e["account"]["data"]) else { continue };
        if d.len() < 324 { continue; }
        let (deps_len, bors_len) = (d[202], d[203]);
        if deps_len != 1 || bors_len != 1 { continue; }        // v1 shape
        let borrowed_value = wad(&d, 90);
        let unhealthy = wad(&d, 122);
        if unhealthy <= 0.0 || borrowed_value <= unhealthy { continue; } // not liquidatable
        if borrowed_value < min_debt { continue; }
        let ratio = borrowed_value / unhealthy;
        if ratio > ratio_cap { continue; } // absurd ratio = mis-priced collateral
        let deposit_reserve = pk(&d, 204);
        let borrow_reserve = pk(&d, 292);
        if borrow_reserve != *usdc_reserve { continue; }        // USDC debt only
        out.push(Candidate { obligation: obl, collateral_reserve: deposit_reserve,
            debt_usd: borrowed_value, ratio });
    }
    out.sort_by(|a, b| b.debt_usd.partial_cmp(&a.debt_usd).unwrap());
    out
}

#[allow(clippy::too_many_arguments)]
fn try_fire(
    endpoint: &str, run_dir: &str, cand: &Candidate, usdc_reserve: &Reserve,
    liquidator_ma: &Pubkey, authority: &Pubkey, kp: Option<&Keypair>, dry_run: bool,
    cfg: &Cfg, sender_url: &str, fresh_bh: solana_hash::Hash, webhook: &Option<String>,
    daily_tip: &std::sync::Arc<std::sync::Mutex<f64>>,
) {
    let mut log = DecisionLog {
        t: now(), obligation: cand.obligation.to_string(), protocol: "save", debt_usd: cand.debt_usd,
        ratio: cand.ratio, repay_native: 0, quoted_usdc_out: 0.0, est_profit_usdc: 0.0, fired: false, reason: String::new(),
    };
    // Fetch full obligation + collateral reserve.
    let Some(obl) = get_acct(endpoint, &cand.obligation).and_then(|d| Obligation::decode(&d)) else {
        log.reason = "obligation refetch/decode failed".into(); log_decision(run_dir, &log); return;
    };
    if obl.deposits.len() != 1 || obl.borrows.len() != 1 { log.reason = "no longer v1".into(); log_decision(run_dir, &log); return; }
    let Some(coll) = get_acct(endpoint, &cand.collateral_reserve).and_then(|d| Reserve::decode(cand.collateral_reserve, &d)) else {
        log.reason = "collateral reserve decode failed".into(); log_decision(run_dir, &log); return;
    };
    let Some(ctp) = mint_owner(endpoint, &coll.liquidity_mint) else {
        log.reason = "collateral token program lookup failed".into(); log_decision(run_dir, &log); return;
    };

    // Size by simulation ladder: largest repay fraction that Solend accepts.
    let mut chosen: Option<(u64, u64, arb_engine::save_fire::SaveFireTx)> = None;
    for frac in &cfg.repay_fracs {
        let repay_usd = cand.debt_usd * frac;
        let repay = (repay_usd / usdc_reserve.market_price.max(1e-9) * 1e6).max(1.0) as u64;
        let seized_usd = repay_usd * (1.0 + coll.liquidation_bonus_pct as f64 / 100.0);
        let seize = (seized_usd / coll.market_price.max(1e-9) * 10f64.powi(coll.mint_decimals as i32)) as u64;
        let c = SaveFireCandidate {
            obligation: cand.obligation, repay_reserve: usdc_reserve.clone(), withdraw_reserve: coll.clone(),
            collateral_token_program: ctp, repay_amount: repay, seize_underlying: seize,
            deposit_reserves: vec![coll.reserve], borrow_reserves: vec![usdc_reserve.reserve],
        };
        let Ok(fire) = build_save_fire_tx(endpoint, &c, liquidator_ma, authority,
            Some(cfg.tip_account), 0, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, solana_hash::Hash::default()) else { continue };
        let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
        if simulate_ok(endpoint, &b64tx) { chosen = Some((repay, seize, fire)); break; }
    }
    let Some((repay, seize, _)) = chosen else {
        log.reason = "no repay fraction passed sim (healthy at fresh price / too small)".into();
        log_decision(run_dir, &log); return;
    };
    log.repay_native = repay;

    // Profit gate: rebuild with the tip, quote USDC out vs repay + tip cost.
    let repay_usd = repay as f64 / 1e6;
    let est_profit_pre = 0.0f64; // filled after rebuild
    let sol_usd = 150.0; // conservative; tip is tiny vs profit
    // First rebuild (no tip) already gave a quote; recompute tip from a fresh build.
    let c = SaveFireCandidate {
        obligation: cand.obligation, repay_reserve: usdc_reserve.clone(), withdraw_reserve: coll.clone(),
        collateral_token_program: ctp, repay_amount: repay, seize_underlying: seize,
        deposit_reserves: vec![coll.reserve], borrow_reserves: vec![usdc_reserve.reserve],
    };
    let Ok(quote_fire) = build_save_fire_tx(endpoint, &c, liquidator_ma, authority,
        Some(cfg.tip_account), 0, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, solana_hash::Hash::default()) else {
        log.reason = "profit rebuild failed".into(); log_decision(run_dir, &log); return;
    };
    let usdc_out = quote_fire.quoted_usdc_out as f64 / 1e6;
    let est_profit = usdc_out - repay_usd;
    let _ = est_profit_pre;
    log.quoted_usdc_out = usdc_out;
    log.est_profit_usdc = est_profit;
    let tip_sol = (est_profit * cfg.tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(cfg.min_tip_sol);
    let tip_lamports = (tip_sol * 1e9) as u64;
    if est_profit < cfg.min_profit + tip_sol * sol_usd {
        log.reason = format!("below min profit (est ${est_profit:.2})"); log_decision(run_dir, &log); return;
    }

    // Final build WITH the tip + a real blockhash, full-sim gate.
    let Ok(mut fire) = build_save_fire_tx(endpoint, &c, liquidator_ma, authority,
        Some(cfg.tip_account), tip_lamports, 100_000, cfg.slippage_bps, cfg.max_swap_accounts, fresh_bh) else {
        log.reason = "final build failed".into(); log_decision(run_dir, &log); return;
    };
    let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
    if !simulate_ok(endpoint, &b64tx) {
        log.reason = "final fire sim revert".into(); log_decision(run_dir, &log); return;
    }

    println!("★ SAVE LIQUIDATABLE {}  debt ${:.0}  repay {}  est profit ${:.2}  tip {:.5} SOL",
        &cand.obligation.to_string()[..8], cand.debt_usd, repay, est_profit, tip_sol);
    if dry_run {
        log.reason = "dry-run: would fire".into(); log.fired = false; log_decision(run_dir, &log);
        alert(webhook, "save-dry", &format!("DRY-RUN Save liquidation {} est profit ${:.2}", cand.obligation, est_profit));
        return;
    }
    if *daily_tip.lock().unwrap() + tip_sol > cfg.max_daily_tip { log.reason = "daily tip cap".into(); log_decision(run_dir, &log); return; }
    if sol_balance(endpoint, &authority.to_string()) < cfg.wallet_min { log.reason = "wallet below floor".into(); log_decision(run_dir, &log); return; }

    let kp = kp.unwrap();
    fire.tx.signatures[0] = kp.sign_message(&fire.tx.message.serialize());
    let sig = fire.tx.signatures[0].to_string();
    let tx_b64 = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
    log.fired = true; log.reason = "fired (sender)".into(); log_decision(run_dir, &log);
    match send_sender(sender_url, &tx_b64) {
        Ok(_) => {
            eprintln!("[save] FIRED {sig}");
            log_trade(run_dir, &TradeLog { t: now(), obligation: cand.obligation.to_string(), protocol: "save",
                repay_native: repay, est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), realized_usdc: None, error: None });
            let (ep, rd, owner, s, wh, tc) = (endpoint.to_string(), run_dir.to_string(), authority.to_string(), sig, webhook.clone(), daily_tip.clone());
            std::thread::spawn(move || {
                for wait in [5u64, 15, 45] {
                    std::thread::sleep(Duration::from_secs(wait));
                    if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                        *tc.lock().unwrap() += tip_sol;
                        log_trade(&rd, &TradeLog { t: now(), obligation: String::new(), protocol: "save", repay_native: 0,
                            est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()), realized_usdc: Some(pnl), error: None });
                        alert(&wh, "save-landed", &format!("Save liquidation landed {s}: realized ${pnl:.2}"));
                        return;
                    }
                }
                alert(&wh, "save-miss", &format!("Save liquidation {s} never confirmed"));
            });
        }
        Err(e) => {
            eprintln!("[save] send failed: {e}");
            log_trade(run_dir, &TradeLog { t: now(), obligation: cand.obligation.to_string(), protocol: "save",
                repay_native: repay, est_profit_usdc: est_profit, tip_lamports, signature: None, realized_usdc: None, error: Some(e.to_string()) });
        }
    }
}

#[derive(Clone)]
struct Cfg {
    tip_account: Pubkey,
    tip_fraction_bps: u64,
    min_tip_sol: f64,
    min_profit: f64,
    max_daily_tip: f64,
    wallet_min: f64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    repay_fracs: Vec<f64>,
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let dry_run = std::env::var("DRY_RUN").map(|s| s != "0").unwrap_or(true);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let min_debt: f64 = std::env::var("MIN_DEBT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let rescan = Duration::from_secs(std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    let max_fire: usize = std::env::var("MAX_FIRE_PER_CYCLE").ok().and_then(|s| s.parse().ok()).unwrap_or(4);
    let ratio_cap: f64 = std::env::var("RATIO_CAP").ok().and_then(|s| s.parse().ok()).unwrap_or(3.0);
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let webhook = std::env::var("ALERT_WEBHOOK").ok();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD".into())).unwrap();
    let repay_fracs: Vec<f64> = std::env::var("REPAY_FRACS").ok()
        .map(|s| s.split(',').filter_map(|x| x.trim().parse().ok()).collect())
        .unwrap_or_else(|| vec![0.2, 0.1, 0.05]);
    let cfg = Cfg {
        tip_account: Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT").unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap(),
        tip_fraction_bps: std::env::var("TIP_FRACTION_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000),
        min_tip_sol: std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002),
        min_profit: std::env::var("MIN_PROFIT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(0.5),
        max_daily_tip: std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05),
        wallet_min: std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02),
        slippage_bps: std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100),
        max_swap_accounts: std::env::var("MAX_SWAP_ACCOUNTS").ok().and_then(|s| s.parse().ok()).unwrap_or(18),
        repay_fracs,
    };

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak".into())).unwrap());

    // USDC reserve decoded once (its accounts are stable).
    let usdc_reserve_pk = Pubkey::from_str(save::USDC_RESERVE).unwrap();
    let usdc_reserve = Reserve::decode(usdc_reserve_pk, &get_acct(&endpoint, &usdc_reserve_pk).expect("usdc reserve"))
        .expect("decode usdc reserve");

    eprintln!("[save] Solend liquidation executor {}  authority={}  min_debt=${min_debt} rescan={:?}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, rescan);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[save] wallet balance: {bal} SOL");
        assert!(bal >= cfg.wallet_min, "wallet below floor");
    }

    let daily_tip = std::sync::Arc::new(std::sync::Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);
    // Cool an obligation after handling so a standing candidate doesn't respin.
    let handle_cd = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30));
    let mut handled: HashMap<Pubkey, Instant> = HashMap::new();

    loop {
        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip.lock().unwrap() = 0.0; }
        if last_bh.elapsed() >= Duration::from_secs(2) {
            if let Some(bh) = latest_blockhash(&endpoint) { fresh_bh = bh; last_bh = Instant::now(); }
        }

        let cands = scan_candidates(&endpoint, &usdc_reserve_pk, min_debt, ratio_cap);
        let fresh: Vec<&Candidate> = cands.iter()
            .filter(|c| handled.get(&c.obligation).is_none_or(|t| t.elapsed() >= handle_cd))
            .take(max_fire).collect();
        eprintln!("[save] scan: {} liquidatable v1 candidates (≥ ${min_debt}), handling {}", cands.len(), fresh.len());
        for c in fresh {
            handled.insert(c.obligation, Instant::now());
            try_fire(&endpoint, &run_dir, c, &usdc_reserve, &liquidator_ma, &authority, kp.as_ref(),
                dry_run, &cfg, &sender_url, fresh_bh, &webhook, &daily_tip);
        }
        std::thread::sleep(rescan);
    }
}
