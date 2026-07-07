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
        if let Some((_f, usd, _t)) = liq::decode_price_update_v2(raw) { by_oracle.insert(*pk, usd); }
    }
    oracle_of.iter().filter_map(|(bk, oc)| Some((*bk, *by_oracle.get(oc)?))).collect()
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

    eprintln!("[exec] marginfi liquidation executor {}  authority={}  min_profit=${}  poll={:?} rescan={:?}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, min_profit, poll, rescan);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[exec] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor {wallet_min_sol}");
    }

    let mut scan: Scan = full_scan(&endpoint).expect("initial scan");
    let mut last_scan = Instant::now();
    let mut watch: Vec<Pubkey> = Vec::new();
    // Counts only LANDED tips (a guard-reverted tx pays no tip — the ix
    // reverts with it), incremented by the readback thread.
    let daily_tip_sol = std::sync::Arc::new(std::sync::Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut mint_tp_cache: HashMap<Pubkey, Pubkey> = HashMap::new();
    // Ladder-rejected candidates (emode phantoms) re-sim at most once per
    // cooldown — they'd otherwise burn 5 gate sims every poll, forever.
    let sim_cooldown = Duration::from_secs(std::env::var("SIM_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(60));
    let mut sim_rejected: HashMap<Pubkey, Instant> = HashMap::new();
    let mut first = true;

    loop {
        // Refresh the watch-set from a full scan.
        if first || last_scan.elapsed() >= rescan {
            if !first {
                if let Some(s) = full_scan(&endpoint) { scan = s; }
            }
            last_scan = Instant::now();
            let prices = fresh_prices(&endpoint, &scan.oracle_of);
            watch = scan.accts.iter().filter_map(|(pk, a)| {
                let r = liq::maintenance_health(a, &scan.banks, &prices);
                (r.missing == 0 && r.health.ratio() >= watch_ratio && r.health.weighted_assets >= min_collateral)
                    .then_some(*pk)
            }).collect();
            eprintln!("[exec] scan: {} borrowers → watch-set {} (ratio ≥ {})", scan.accts.len(), watch.len(), watch_ratio);
            first = false;
        }

        // Fast poll: fresh watch-set accounts + prices.
        let fresh_raw = get_multiple(&endpoint, &watch);
        let prices = fresh_prices(&endpoint, &scan.oracle_of);
        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip_sol.lock().unwrap() = 0.0; }

        for pk in &watch {
            let Some(a) = fresh_raw.get(pk).and_then(|raw| MarginfiAccount::decode(raw)) else { continue };
            let r = liq::maintenance_health(&a, &scan.banks, &prices);
            if r.missing > 0 || !r.health.liquidatable() || r.health.weighted_assets < min_collateral { continue; }
            let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).cloned().collect();
            let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).cloned().collect();
            if assets.len() != 1 || liabs.len() != 1 || liabs[0].bank_pk != usdc_bank { continue; }
            if sim_rejected.get(pk).is_some_and(|t| t.elapsed() < sim_cooldown) { continue; }
            let asset_bank = assets[0].bank_pk;
            let bank = &scan.banks[&asset_bank];
            let native_total = assets[0].asset_shares * bank.asset_share_value;

            // Size by simulation ladder, largest first.
            let mut seize = 0u64;
            for frac in SIZE_LADDER {
                let amount = (native_total * frac) as u64;
                if amount == 0 { continue; }
                if simulate_gate(&endpoint, &authority, &liquidator_ma, &tp, pk, &a, asset_bank, usdc_bank, amount, &scan.oracle_of) == Some(true) {
                    seize = amount;
                    break;
                }
            }
            if seize == 0 { sim_rejected.insert(*pk, Instant::now()); continue; } // sim said healthy (emode phantom)
            sim_rejected.remove(pk);

            // Fire candidate: build the atomic tx (Jupiter quote inside).
            let asset_tp = match mint_tp_cache.get(&bank.mint) {
                Some(t) => *t,
                None => {
                    let Some(t) = mint_owner(&endpoint, &bank.mint) else { continue };
                    mint_tp_cache.insert(bank.mint, t);
                    t
                }
            };
            let mut obs = Vec::new();
            for b in &a.balances {
                let Some(oc) = scan.oracle_of.get(&b.bank_pk) else { continue };
                obs.push(AccountMeta::new_readonly(b.bank_pk, false));
                obs.push(AccountMeta::new_readonly(*oc, false));
            }
            let cand = FireCandidate {
                liquidatee: *pk, asset_bank, asset_mint: bank.mint, asset_token_program: asset_tp,
                asset_amount: seize, liab_bank: usdc_bank,
                asset_oracle: scan.oracle_of[&asset_bank], liab_oracle: scan.oracle_of[&usdc_bank],
                liquidatee_obs: obs,
            };

            // Profit estimate: we take on ~97.5% of the seized value as USDC
            // liability (2.5% liquidator bonus); the swap quote is what we get.
            let price = prices.get(&asset_bank).copied().unwrap_or(0.0);
            let seized_usd = seize as f64 / 10f64.powi(bank.mint_decimals as i32) * price;
            let est_liab = seized_usd * 0.975;
            // Tip: fraction of estimated profit, floored at Sender's minimum.
            let sol_usd = scan.banks.iter()
                .find(|(_, b)| b.mint.to_string() == SOL_MINT)
                .and_then(|(bk, _)| prices.get(bk)).copied().unwrap_or(150.0);
            let bh = latest_blockhash(&endpoint).unwrap_or_default();

            let mut log = DecisionLog {
                t: now(), liquidatee: pk.to_string(), collateral_usd: r.health.weighted_assets,
                ratio: r.health.ratio(), seize_native: seize, quoted_usdc_out: 0.0,
                est_liab_usdc: est_liab, est_profit_usdc: 0.0, fire_sim_ok: false, fired: false,
                reason: String::new(),
            };
            let fire = match liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority,
                Some(tip_account), 0 /* patched below via rebuild */, 100_000, slippage_bps, 20, bh) {
                Ok(f) => f,
                Err(e) => { log.reason = format!("build: {e}"); log_decision(&run_dir, &log); continue; }
            };
            log.quoted_usdc_out = fire.quoted_usdc_out as f64 / 1e6;
            let est_profit = fire.quoted_usdc_out as f64 / 1e6 - est_liab;
            log.est_profit_usdc = est_profit;
            let tip_sol = (est_profit * tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(min_tip_sol);
            let tip_lamports = (tip_sol * 1e9) as u64;
            if est_profit < min_profit + tip_sol * sol_usd {
                log.reason = format!("below min profit (est ${est_profit:.2}, tip ${:.2})", tip_sol * sol_usd);
                log_decision(&run_dir, &log); continue;
            }
            // Rebuild with the real tip (build is quote-dependent; keep it fresh).
            let fire = match liq_fire::build_fire_tx(&endpoint, &cand, &liquidator_ma, &authority,
                Some(tip_account), tip_lamports, 100_000, slippage_bps, 20, bh) {
                Ok(f) => f,
                Err(e) => { log.reason = format!("rebuild: {e}"); log_decision(&run_dir, &log); continue; }
            };

            // Ground-truth gate: full fire-tx simulation (every leg incl. swap+repay).
            let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
            let sim_ok = simulate_tx_b64(&endpoint, &b64tx).map(|res| res["err"].is_null()).unwrap_or(false);
            log.fire_sim_ok = sim_ok;
            if !sim_ok {
                log.reason = "fire-tx sim revert (swap/repay would not cover liability)".into();
                log_decision(&run_dir, &log);
                continue;
            }

            println!("★ LIQUIDATABLE  {}  seize {} native (${:.0})  quoted out ${:.2}  est profit ${:.2}  tip {:.5} SOL",
                &pk.to_string()[..8], seize, seized_usd, log.quoted_usdc_out, est_profit, tip_sol);

            if dry_run {
                log.fired = false;
                log.reason = "dry-run: would fire".into();
                log_decision(&run_dir, &log);
                alert(&webhook, "liq-dry", &format!("DRY-RUN liquidation: {} est profit ${:.2}", pk, est_profit));
                continue;
            }
            if *daily_tip_sol.lock().unwrap() + tip_sol > max_daily_tip_sol {
                log.reason = "daily tip cap".into();
                log_decision(&run_dir, &log);
                alert(&webhook, "liq-cap", "daily tip cap reached");
                continue;
            }
            if sol_balance(&endpoint, &authority.to_string()) < wallet_min_sol {
                log.reason = "wallet below floor".into();
                log_decision(&run_dir, &log);
                alert(&webhook, "liq-floor", "wallet below floor — not firing");
                continue;
            }

            let mut tx = fire.tx;
            let kp = kp.as_ref().unwrap();
            tx.signatures[0] = kp.sign_message(&tx.message.serialize());
            let sig = tx.signatures[0].to_string();
            let tx_b64 = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap()) };
            log.fired = true;
            log.reason = "fired".into();
            log_decision(&run_dir, &log);
            match send_sender(&sender_url, &tx_b64) {
                Ok(_) => {
                    eprintln!("[exec] FIRED {sig}");
                    log_trade(&run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                        est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), realized_usdc: None, error: None });
                    // Detached P&L readback.
                    let (ep, rd, owner, s, wh) = (endpoint.clone(), run_dir.clone(), authority.to_string(), sig, webhook.clone());
                    let tip_counter = daily_tip_sol.clone();
                    std::thread::spawn(move || {
                        for wait in [5u64, 15, 45] {
                            std::thread::sleep(Duration::from_secs(wait));
                            if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                                *tip_counter.lock().unwrap() += tip_sol;
                                log_trade(&rd, &TradeLog { t: now(), liquidatee: String::new(), seize_native: 0,
                                    est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()),
                                    realized_usdc: Some(pnl), error: None });
                                alert(&wh, "liq-landed", &format!("liquidation landed {s}: realized ${pnl:.2}"));
                                return;
                            }
                        }
                        alert(&wh, "liq-miss", &format!("liquidation {s} never confirmed"));
                    });
                }
                Err(e) => {
                    eprintln!("[exec] send failed: {e}");
                    log_trade(&run_dir, &TradeLog { t: now(), liquidatee: pk.to_string(), seize_native: seize,
                        est_profit_usdc: est_profit, tip_lamports, signature: None, realized_usdc: None, error: Some(e.to_string()) });
                }
            }
        }
        std::thread::sleep(poll);
    }
}
