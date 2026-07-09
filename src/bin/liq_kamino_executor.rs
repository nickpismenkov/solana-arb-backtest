//! Production Kamino (KLend) liquidation executor — continuous loop, DRY_RUN
//! default. Same discipline as the marginfi executor: the FULL fire-tx
//! simulation is the ground-truth liquidatable+profitable gate (Kamino runs its
//! own refresh + health inside the tx), so we never trust off-chain health for
//! the fire decision — only to build the watch-set cheaply.
//!
//!   full scan (RESCAN_SECS): decode obligations + reserves → recompute health
//!     → watch-set (stored/recomputed ratio ≥ WATCH_RATIO, value ≥ min)
//!   poll (POLL_MS): re-fetch watch-set obligations + their reserves fresh
//!     → recompute → for each near/over-threshold, USDC-debt, single-position:
//!        size repay (close factor) → build fire tx → FULL sim gate
//!        → profit gate (quoted USDC out vs borrow + tip) → DRY_RUN log / fire
//!
//! v1: USDC-debt, single-deposit/single-borrow, non-elevation obligations
//! (the fire path + finder handle exactly these; others are logged + skipped).
//!
//! Usage: HELIUS_RPC=<url> [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json]
//!        [MIN_DEBT_USD=100] [MIN_PROFIT_USD=0.5] [CLOSE_FACTOR=0.2]
//!        [POLL_MS=5000] [RESCAN_SECS=300] [WATCH_RATIO=0.9] [RUN_DIR=runs]
//!        cargo run --release --bin liq_kamino_executor

use arb_engine::jito::send_sender;
use arb_engine::kamino::{self, Obligation, Reserve};
use arb_engine::kamino_fire::{self, KaminoFireCandidate};
use arb_engine::kamino_ix::ReserveAccounts;
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use serde::Serialize;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

const KLEND: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
const MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";
const SOL_MINT: &str = "So11111111111111111111111111111111111111112";
const TOKEN_PROGRAM: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const OBLIGATION_SIZE: usize = 3344;
const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";

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
fn mint_owner(endpoint: &str, mint: &Pubkey, cache: &mut HashMap<Pubkey, Pubkey>) -> Pubkey {
    if let Some(p) = cache.get(mint) { return *p; }
    let p = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[mint.to_string(),{"encoding":"base64"}]}))
        .and_then(|v| v["result"]["value"]["owner"].as_str().map(String::from))
        .and_then(|s| Pubkey::from_str(&s).ok())
        .unwrap_or_else(|| Pubkey::from_str(TOKEN_PROGRAM).unwrap());
    cache.insert(*mint, p);
    p
}
fn latest_blockhash(endpoint: &str) -> solana_hash::Hash {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"finalized"}]}))
        .and_then(|v| v["result"]["value"]["blockhash"].as_str().map(String::from))
        .and_then(|s| solana_hash::Hash::from_str(&s).ok()).unwrap_or_default()
}
fn sol_balance(endpoint: &str, owner: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[owner]}))
        .and_then(|v| v["result"]["value"].as_u64()).map(|l| l as f64 / 1e9).unwrap_or(0.0)
}
fn sim_err_null(endpoint: &str, b64tx: &str) -> Option<bool> {
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))?;
    Some(sim.get("result")?.get("value")?["err"].is_null())
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

fn scan_obligations(endpoint: &str) -> Vec<(Pubkey, Obligation)> {
    let resp = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[KLEND, {"encoding":"base64","dataSlice":{"offset":0,"length":2288},
            "filters":[{"dataSize":OBLIGATION_SIZE},{"memcmp":{"offset":32,"bytes":MAIN_MARKET}}]}]}));
    resp.as_ref().and_then(|v| v["result"].as_array()).cloned().unwrap_or_default().iter().filter_map(|e| {
        Some((e["pubkey"].as_str()?.parse().ok()?, Obligation::decode(&b64(&e["account"]["data"])?)?))
    }).filter(|(_, o): &(Pubkey, Obligation)| !o.borrows.is_empty()).collect()
}

fn fetch_reserves(endpoint: &str, pks: &[Pubkey]) -> HashMap<Pubkey, Reserve> {
    get_multiple(endpoint, pks).iter().filter_map(|(pk, raw)| Some((*pk, Reserve::decode(raw)?))).collect()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let dry_run = std::env::var("DRY_RUN").map(|s| s != "0").unwrap_or(true);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let min_debt: f64 = std::env::var("MIN_DEBT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(100.0);
    let min_profit: f64 = std::env::var("MIN_PROFIT_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(0.5);
    let close_factor: f64 = std::env::var("CLOSE_FACTOR").ok().and_then(|s| s.parse().ok()).unwrap_or(0.2);
    let tip_fraction_bps: u64 = std::env::var("TIP_FRACTION_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(3000);
    let min_tip_sol: f64 = std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let poll = Duration::from_millis(std::env::var("POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(5000));
    let rescan = Duration::from_secs(std::env::var("RESCAN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(300));
    let watch_ratio: f64 = std::env::var("WATCH_RATIO").ok().and_then(|s| s.parse().ok()).unwrap_or(0.9);
    let slippage_bps: u32 = std::env::var("SLIPPAGE_BPS").ok().and_then(|s| s.parse().ok()).unwrap_or(100);
    let max_borrow_usd: f64 = std::env::var("MAX_BORROW_USD").ok().and_then(|s| s.parse().ok()).unwrap_or(5000.0);
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let tip_account = Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT")
        .unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap();
    let webhook = std::env::var("ALERT_WEBHOOK").ok();
    let market = Pubkey::from_str(MAIN_MARKET).unwrap();

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey())
        .unwrap_or_else(|| Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap());

    eprintln!("[kexec] Kamino liquidation executor {}  authority={}  min_profit=${}  poll={:?} rescan={:?}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, authority, min_profit, poll, rescan);
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[kexec] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor {wallet_min_sol}");
    }

    let mut obligations = scan_obligations(&endpoint);
    let mut reserves = fetch_reserves(&endpoint,
        &obligations.iter().flat_map(|(_, o)| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
            .collect::<HashSet<_>>().into_iter().collect::<Vec<_>>());
    let mut watch: Vec<Pubkey> = Vec::new();
    let mut last_scan = Instant::now();
    let mut first = true;
    let daily_tip_sol = Arc::new(Mutex::new(0.0f64));
    let mut tip_day = now() / 86_400;
    let mut tp_cache: HashMap<Pubkey, Pubkey> = HashMap::new();
    let mut ob_index: HashMap<Pubkey, Obligation> = obligations.iter().cloned().collect();

    loop {
        if first || last_scan.elapsed() >= rescan {
            if !first {
                obligations = scan_obligations(&endpoint);
                reserves = fetch_reserves(&endpoint,
                    &obligations.iter().flat_map(|(_, o)| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
                        .collect::<HashSet<_>>().into_iter().collect::<Vec<_>>());
                ob_index = obligations.iter().cloned().collect();
            }
            last_scan = Instant::now();
            watch = obligations.iter().filter_map(|(pk, o)| {
                let r = kamino::recompute(o, &reserves);
                (r.trustworthy() && r.ratio() >= watch_ratio && r.unhealthy_borrow_value >= min_debt).then_some(*pk)
            }).collect();
            eprintln!("[kexec] scan: {} obligations → watch-set {} (ratio ≥ {})", obligations.len(), watch.len(), watch_ratio);
            first = false;
        }

        // Fresh reserves (prices move; obligations rarely change between scans).
        let reserve_pks: Vec<Pubkey> = watch.iter().filter_map(|pk| ob_index.get(pk))
            .flat_map(|o| o.deposits.iter().map(|d| d.0).chain(o.borrows.iter().map(|b| b.0)))
            .collect::<HashSet<_>>().into_iter().collect();
        if !reserve_pks.is_empty() { reserves.extend(fetch_reserves(&endpoint, &reserve_pks)); }
        let day = now() / 86_400;
        if day != tip_day { tip_day = day; *daily_tip_sol.lock().unwrap() = 0.0; }
        // SOL/USD for USDC-profit → SOL-tip conversion: a 9-decimal reserve
        // priced in the SOL range. Falls back to $150 if none is loaded.
        let sol_usd = reserves.values()
            .find_map(|r| (r.mint_decimals == 9 && (20.0..2000.0).contains(&r.market_price)).then_some(r.market_price))
            .unwrap_or(150.0);
        let _ = SOL_MINT;

        for pk in &watch {
            let Some(o) = ob_index.get(pk) else { continue };
            if o.deposits.len() != 1 || o.borrows.len() != 1 || o.elevation_group != 0 { continue; }
            let r = kamino::recompute(o, &reserves);
            if !r.trustworthy() || r.ratio() < 1.0 || r.unhealthy_borrow_value < min_debt { continue; }
            let withdraw_pk = o.deposits[0].0;
            let repay_pk = o.borrows[0].0;
            let (Some(rr_res), Some(wr_res)) = (reserves.get(&repay_pk), reserves.get(&withdraw_pk)) else { continue };

            let raw = get_multiple(&endpoint, &[withdraw_pk, repay_pk]);
            let (Some(wr), Some(rr)) = (
                raw.get(&withdraw_pk).and_then(|d| ReserveAccounts::decode(withdraw_pk, d)),
                raw.get(&repay_pk).and_then(|d| ReserveAccounts::decode(repay_pk, d)),
            ) else { continue };
            // v1.5: any debt with a wired JupLend flash market (USDC/USDT/wSOL).
            if !arb_engine::flashloan::has_market(&rr.liquidity_mint) { continue; }
            let debt_dec = rr_res.mint_decimals as i32;
            let debt_price = rr_res.market_price.max(1e-9);

            let debt_usd = (o.borrows[0].1 / 10f64.powi(debt_dec)) * rr_res.market_price;
            let repay_usd = (debt_usd * close_factor).min(max_borrow_usd).max(1.0);
            // Native debt units to borrow/repay: price the USD close amount in the
            // actual debt asset (was hardcoded USDC 1e6/$1).
            let repay_amount = (repay_usd / debt_price * 10f64.powi(debt_dec)) as u64;
            let bonus = 1.05;
            let seized_native = repay_usd * bonus / wr_res.market_price * 10f64.powi(wr_res.mint_decimals as i32);
            let swap_in_amount = (seized_native * 0.995) as u64;

            let mut log = DecisionLog {
                t: now(), obligation: pk.to_string(), protocol: "kamino", ratio: r.ratio(),
                debt_usd, repay_usd, quoted_usdc_out: 0.0, est_profit_usdc: 0.0,
                fire_sim_ok: false, fired: false, reason: String::new(),
            };
            let cand = KaminoFireCandidate {
                obligation: *pk, lending_market: market, repay_reserve: rr.clone(), withdraw_reserve: wr.clone(),
                obligation_reserves: vec![withdraw_pk, repay_pk],
                withdraw_liquidity_mint: wr.liquidity_mint,
                withdraw_liquidity_token_program: mint_owner(&endpoint, &wr.liquidity_mint, &mut tp_cache),
                withdraw_collateral_token_program: mint_owner(&endpoint, &wr.collateral_mint, &mut tp_cache),
                repay_liquidity_token_program: mint_owner(&endpoint, &rr.liquidity_mint, &mut tp_cache),
                repay_amount, swap_in_amount,
            };
            let bh = latest_blockhash(&endpoint);
            let est_profit_pre = 0.0; // filled after build
            let tip_sol = min_tip_sol; // provisional; recomputed from profit below
            let fire = match kamino_fire::build_fire_tx(&endpoint, &cand, &authority,
                Some(tip_account), (tip_sol * 1e9) as u64, 100_000, slippage_bps, 20, bh) {
                Ok(f) => f,
                Err(e) => { log.reason = format!("build: {e}"); log_decision(&run_dir, &log); continue; }
            };
            let _ = est_profit_pre;
            let quoted_usd = fire.quoted_usdc_out as f64 / 10f64.powi(debt_dec) * debt_price;
            log.quoted_usdc_out = quoted_usd;
            let est_profit = quoted_usd - repay_usd;
            log.est_profit_usdc = est_profit;
            let tip_sol = (est_profit * tip_fraction_bps as f64 / 10_000.0 / sol_usd).max(min_tip_sol);
            if est_profit < min_profit + tip_sol * sol_usd {
                log.reason = format!("below min profit (est ${est_profit:.2}, tip ${:.2})", tip_sol * sol_usd);
                log_decision(&run_dir, &log); continue;
            }
            // Rebuild with the real tip and full-sim gate.
            let tip_lamports = (tip_sol * 1e9) as u64;
            let fire = match kamino_fire::build_fire_tx(&endpoint, &cand, &authority,
                Some(tip_account), tip_lamports, 100_000, slippage_bps, 20, bh) {
                Ok(f) => f,
                Err(e) => { log.reason = format!("rebuild: {e}"); log_decision(&run_dir, &log); continue; }
            };
            let b64tx = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).unwrap()) };
            let sim_ok = sim_err_null(&endpoint, &b64tx).unwrap_or(false);
            log.fire_sim_ok = sim_ok;
            if !sim_ok {
                log.reason = "fire-tx sim revert (not liquidatable at refreshed prices, or swap short)".into();
                log_decision(&run_dir, &log); continue;
            }

            println!("★ KAMINO LIQUIDATABLE {}  debt ${:.0}  repay ${:.2}  quoted ${:.2}  est profit ${:.2}  tip {:.5} SOL  ({} bytes)",
                &pk.to_string()[..8], debt_usd, repay_usd, log.quoted_usdc_out, est_profit, tip_sol, fire.tx_bytes);

            if dry_run {
                log.reason = "dry-run: would fire".into();
                log_decision(&run_dir, &log);
                alert(&webhook, "kliq-dry", &format!("DRY-RUN Kamino liq {} est profit ${:.2}", pk, est_profit));
                continue;
            }
            if *daily_tip_sol.lock().unwrap() + tip_sol > max_daily_tip_sol {
                log.reason = "daily tip cap".into(); log_decision(&run_dir, &log);
                alert(&webhook, "kliq-cap", "daily tip cap reached"); continue;
            }
            if sol_balance(&endpoint, &authority.to_string()) < wallet_min_sol {
                log.reason = "wallet below floor".into(); log_decision(&run_dir, &log);
                alert(&webhook, "kliq-floor", "wallet below floor"); continue;
            }

            let mut tx = fire.tx;
            let kp = kp.as_ref().unwrap();
            tx.signatures[0] = kp.sign_message(&tx.message.serialize());
            let sig = tx.signatures[0].to_string();
            let tx_b64 = { use base64::Engine; base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap()) };
            log.fired = true; log.reason = "fired".into();
            log_decision(&run_dir, &log);
            match send_sender(&sender_url, &tx_b64) {
                Ok(_) => {
                    eprintln!("[kexec] FIRED {sig}");
                    log_trade(&run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "kamino",
                        repay_usd, est_profit_usdc: est_profit, tip_lamports, signature: Some(sig.clone()), realized_usdc: None, error: None });
                    let (ep, rd, owner, s, wh, tip_counter) =
                        (endpoint.clone(), run_dir.clone(), authority.to_string(), sig, webhook.clone(), daily_tip_sol.clone());
                    std::thread::spawn(move || {
                        for wait in [5u64, 15, 45] {
                            std::thread::sleep(Duration::from_secs(wait));
                            if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                                *tip_counter.lock().unwrap() += tip_sol;
                                log_trade(&rd, &TradeLog { t: now(), obligation: String::new(), protocol: "kamino",
                                    repay_usd: 0.0, est_profit_usdc: 0.0, tip_lamports: 0, signature: Some(s.clone()),
                                    realized_usdc: Some(pnl), error: None });
                                alert(&wh, "kliq-landed", &format!("Kamino liq landed {s}: realized ${pnl:.2}"));
                                return;
                            }
                        }
                        alert(&wh, "kliq-miss", &format!("Kamino liq {s} never confirmed"));
                    });
                }
                Err(e) => {
                    eprintln!("[kexec] send failed: {e}");
                    log_trade(&run_dir, &TradeLog { t: now(), obligation: pk.to_string(), protocol: "kamino",
                        repay_usd, est_profit_usdc: est_profit, tip_lamports, signature: None, realized_usdc: None, error: Some(e.to_string()) });
                }
            }
        }
        std::thread::sleep(poll);
    }
}
