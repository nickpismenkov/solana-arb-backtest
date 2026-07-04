//! Arb executor v2 (pragmatic fast reactor, blind-guarded fire). Hot path is
//! memory reads + sign + submit ONLY — no RPC, no disk, no network calls in
//! the reaction. Slow work on background threads:
//!   - RPC poll (~10s) → PoolData cache (pool accounts for building)
//!   - RPC poll (~20s) → recent blockhash
//!   - config hot-reload (~3s) → Arc<RwLock<Config>> (pause / size / tip)
//!   - log writer thread ← mpsc channel (decisions/trades JSONL, off hot path)
//!   - realized-P&L readback (detached, later)
//!
//! On a shred trigger (not paused): build guarded arb from cached state +
//! blockhash, sign, submit to Jito. The exact-out leg-2 guard is the real
//! profitability check — unprofitable txs revert for free, tips only pay on
//! wins. No price filtering; every trigger fires unless paused/dry_run.
//! DRY_RUN=1 (default) logs and never submits.
//!
//! Env: RPC_ENDPOINT, ALT_ADDRESS, KEYPAIR_PATH, SHREDSTREAM_PORT, RUN_DIR,
//!      DRY_RUN, CONFIG_PATH, JITO_BLOCK_ENGINE, WALLET_MIN_SOL,
//!      MAX_DAILY_TIP_SOL, ALERT_WEBHOOK.

use arb_engine::arb::{build_arb_tx, load_alt, PoolData};
use arb_engine::clmm::{optimal_arb, wsol, ClmmState};
use arb_engine::decode::Dir;
use arb_engine::jito::{default_block_engine, get_tip_accounts, send_sender};
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use arb_engine::pools::pair;
use base64::Engine;
use serde::{Deserialize, Serialize};
use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_message::AddressLookupTableAccount;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use std::str::FromStr;
use std::sync::{Arc, RwLock};
use std::time::{Duration, Instant};

#[derive(Clone, Deserialize)]
struct Config {
    #[serde(default)]
    paused: bool,
    #[serde(default = "d_borrow")]
    borrow_usdc: f64,
    #[serde(default = "d_priority")]
    priority_micro_lamports: u64,
    /// Tip as a fraction of computed profit (bps). Jito's auction is won by
    /// paying a fraction of profit; capped at 80% so we always net positive.
    #[serde(default = "d_tip_frac")]
    tip_fraction_bps: u64,
    /// Minimum computed profit (lamports) to fire. Must clear tip + fees.
    #[serde(default = "d_min_profit")]
    min_profit_lamports: u64,
}
fn d_borrow() -> f64 { 500.0 }
fn d_priority() -> u64 { 10_000 }
fn d_tip_frac() -> u64 { 3000 } // 30% of computed profit
fn d_min_profit() -> u64 { 500_000 } // 0.0005 SOL; must clear Sender's 0.0002 tip floor + fees + buffer
const SENDER_MIN_TIP: f64 = 200_000.0; // Helius Sender requires ≥0.0002 SOL tip
// SOL/USDC Orca pool (SOL=mintA/dec9, USDC=mintB/dec6) — independent SOL price
// reference for USDC→SOL tip conversion, regardless of the traded pair.
const SOL_USDC_REF: &str = "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE";
impl Default for Config {
    fn default() -> Self {
        Self { paused: false, borrow_usdc: d_borrow(), priority_micro_lamports: d_priority(), tip_fraction_bps: d_tip_frac(), min_profit_lamports: d_min_profit() }
    }
}

#[derive(Serialize)]
struct DecisionLog { t: u64, venue: &'static str, slot: u64, fired: bool, reason: &'static str }
#[derive(Serialize)]
struct TradeLog { t: u64, borrow_usdc: f64, tip_lamports: u64, bundle_id: Option<String>, signature: Option<String>, bundle_status: Option<String>, realized_usdc: Option<f64>, error: Option<String> }

enum LogMsg { Decision(DecisionLog), Trade(TradeLog) }

fn now() -> u64 {
    use std::time::{SystemTime, UNIX_EPOCH};
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs()
}

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..3 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(300 << attempt));
    }
    None
}
fn account_data(endpoint: &str, addr: &str) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[addr,{"encoding":"base64"}]}))?;
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"][0].as_str()?).ok()
}
fn latest_blockhash(endpoint: &str) -> Option<Hash> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"confirmed"}]}))?;
    Hash::from_str(v["result"]["value"]["blockhash"].as_str()?).ok()
}
fn sol_balance(endpoint: &str, pk: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[pk]})).and_then(|v| v["result"]["value"].as_f64()).unwrap_or(0.0) / 1e9
}

fn load_config(path: &str) -> Config {
    std::fs::read_to_string(path).ok().and_then(|s| serde_json::from_str(&s).ok()).unwrap_or_default()
}


fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let alt_addr = std::env::var("ALT_ADDRESS").expect("ALT_ADDRESS");
    let port: u16 = std::env::var("SHREDSTREAM_PORT").ok().and_then(|s| s.parse().ok()).unwrap_or(20000);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let dry_run = std::env::var("DRY_RUN").map(|v| v != "0").unwrap_or(true);
    let config_path = std::env::var("CONFIG_PATH").unwrap_or_else(|_| "arb.config.json".into());
    let block_engine = default_block_engine();
    // Helius Sender: fast dual-route landing (validators + Jito), no 1/sec cap.
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let pace_ms: u64 = std::env::var("PACE_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(250);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let webhook = std::env::var("ALERT_WEBHOOK").ok();
    let cfg = pair();

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run { panic!("LIVE needs KEYPAIR_PATH"); }
    let signer = kp.as_ref().map(|k| k.pubkey()).unwrap_or_else(|| Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap());

    // Static, one-time: ALT + tip accounts.
    let alt: Arc<AddressLookupTableAccount> = Arc::new(load_alt(&alt_addr, &account_data(&endpoint, &alt_addr).expect("ALT")));
    let tip_accounts = get_tip_accounts(&block_engine).unwrap_or_default();
    let tip_account = tip_accounts.first().copied();

    // Shared caches.
    let pooldata: Arc<RwLock<Option<PoolData>>> = Arc::new(RwLock::new(None));
    let blockhash = Arc::new(RwLock::new(Hash::default()));
    let config = Arc::new(RwLock::new(load_config(&config_path)));
    // SOL/USDC reference price (USDC per SOL) for converting USDC profit → SOL
    // tip. When the traded base ISN'T SOL (e.g. SPYx), the trading pool's price
    // is the wrong denominator; we always convert via this independent SOL feed.
    let sol_usd = Arc::new(RwLock::new(0.0f64));

    // Seed pool data + blockhash + SOL price before starting.
    if let (Some(o), Some(r)) = (account_data(&endpoint, &cfg.orca_pool), account_data(&endpoint, &cfg.ray_pool)) {
        *pooldata.write().unwrap() = Some(PoolData { orca: o, ray: r });
    }
    if let Some(bh) = latest_blockhash(&endpoint) { *blockhash.write().unwrap() = bh; }
    if let Some(d) = account_data(&endpoint, SOL_USDC_REF) {
        if let Some(s) = ClmmState::from_orca(&d, 9, 6, 4.0) { *sol_usd.write().unwrap() = s.ui_price(); }
    }

    eprintln!(
        "executor v2 {} pair={} alt={} wallet={} dry_run={} — hot path: blind-guarded fire",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" }, cfg.label, &alt_addr[..8], signer, dry_run
    );
    if !dry_run {
        let bal = sol_balance(&endpoint, &signer.to_string());
        eprintln!("wallet balance: {bal} SOL");
        if bal < wallet_min_sol { panic!("wallet below floor {wallet_min_sol}"); }
    }

    // ── background: pool data (12s) + blockhash (3s) refresh ──
    // Blockhash refreshes frequently because Jito rejects expired blockhashes.
    // Falls back to a secondary RPC if the primary fails (the shredstream
    // feed's ALT fetches share the primary and can rate-limit it).
    {
        let (ep, pd, bh, su) = (endpoint.clone(), pooldata.clone(), blockhash.clone(), sol_usd.clone());
        let fb = std::env::var("RPC_FALLBACK")
            .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".into());
        let (op, rp) = (cfg.orca_pool.clone(), cfg.ray_pool.clone());
        // Pool state drives the profit prediction, so keep it as fresh as the
        // RPC allows (POOL_POLL_MS, default 1s). A stale snapshot quantises the
        // predicted profit to the refresh cycle — visible as identical profits
        // across distinct victims. Blockhash only needs ~every few seconds.
        let poll_ms: u64 = std::env::var("POOL_POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1000);
        std::thread::spawn(move || {
            let mut tick = 0u64;
            let mut bh_fails = 0u64;
            loop {
                std::thread::sleep(Duration::from_millis(poll_ms));
                // Pool state EVERY tick (freshness matters most).
                let (o, r) = (
                    account_data(&ep, &op).or_else(|| account_data(&fb, &op)),
                    account_data(&ep, &rp).or_else(|| account_data(&fb, &rp)),
                );
                if let (Some(o), Some(r)) = (o, r) {
                    *pd.write().unwrap() = Some(PoolData { orca: o, ray: r });
                } else {
                    eprintln!("[warn] pool data refresh failed on both endpoints");
                }
                // Blockhash + SOL/USDC reference price roughly every 3s.
                tick += 1;
                if tick % (3000 / poll_ms).max(1) == 0 {
                    match latest_blockhash(&ep).or_else(|| latest_blockhash(&fb)) {
                        Some(h) => { bh_fails = 0; *bh.write().unwrap() = h; }
                        None => { bh_fails += 1; eprintln!("[warn] blockhash refresh failed on BOTH endpoints ({bh_fails} in a row)"); }
                    }
                    if let Some(d) = account_data(&ep, SOL_USDC_REF).or_else(|| account_data(&fb, SOL_USDC_REF)) {
                        if let Some(s) = ClmmState::from_orca(&d, 9, 6, 4.0) { *su.write().unwrap() = s.ui_price(); }
                    }
                }
            }
        });
    }

    // ── background: config hot-reload (3s) ──
    {
        let (cfgp, conf) = (config_path.clone(), config.clone());
        std::thread::spawn(move || loop {
            std::thread::sleep(Duration::from_secs(3));
            *conf.write().unwrap() = load_config(&cfgp);
        });
    }

    // ── background: log writer (channel → JSONL), OFF hot path ──
    let (log_tx, log_rx) = std::sync::mpsc::channel::<LogMsg>();
    {
        let rd = run_dir.clone();
        std::thread::spawn(move || {
            for msg in log_rx {
                match msg {
                    LogMsg::Decision(d) => log_decision(&rd, &d),
                    LogMsg::Trade(t) => log_trade(&rd, &t),
                }
            }
        });
    }

    // ── shred trigger feed → std channel bridge ──
    let (trig_tx, trig_rx) = std::sync::mpsc::channel();
    let (feed_tx, mut feed_rx) = tokio::sync::mpsc::unbounded_channel();
    let _feed = arb_engine::shredstream::run_shredstream_feed(port, Some(endpoint.clone()), feed_tx);
    std::thread::spawn(move || {
        let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
        rt.block_on(async move { while let Some(tg) = feed_rx.recv().await { if trig_tx.send(tg).is_err() { break; } } });
    });

    let daily_tip_sol = Arc::new(RwLock::new(0.0f64));
    let (mut triggers, mut fired) = (0u64, 0u64);

    let base = wsol();
    let mut seen_sigs: std::collections::HashSet<String> = std::collections::HashSet::new();
    // Jito's unauthenticated lane hard-limits to 1 bundle/sec — firing faster
    // just 429s. Pace to ~1/sec (an auth key would lift this).
    let mut last_submit = Instant::now() - Duration::from_secs(10);
    // ═══ HOT PATH ═══ decode victim → predict exact profit → gate → co-bundle.
    // All arithmetic on cached state; the only network call is the Jito submit.
    for trigger in trig_rx {
        triggers += 1;
        let c = config.read().unwrap().clone();
        if c.paused { continue; }

        // Only co-bundle DECODABLE direct victims. Routed/CPI swaps decode to
        // empty (we can't predict their pool effect) → skip silently (logging
        // every such trigger would flood the ledger; they're the majority).
        let Some(victim) = trigger.swaps.iter().find(|s| s.amount_is_input && s.amount > 0).cloned() else {
            continue;
        };

        let bh = *blockhash.read().unwrap();
        let pd_guard = pooldata.read().unwrap();
        let Some(ref pd) = *pd_guard else { continue };
        // Decode both pools. Orca decimals from mintA (offset 101); Ray self-describes.
        let orca_mint_a = Pubkey::try_from(&pd.orca[101..133]).ok();
        let (oda, odb) = match orca_mint_a { Some(m) if m == base => (cfg.base_dec, cfg.quote_dec), _ => (cfg.quote_dec, cfg.base_dec) };
        let (Some(orca0), Some(ray0)) = (ClmmState::from_orca(&pd.orca, oda, odb, cfg.orca_fee_bps), ClmmState::from_ray(&pd.ray, cfg.ray_fee_bps)) else { continue };

        // Apply the victim's swap to the pool it hits → predicted post-victim state.
        let sell_base = victim.dir == Dir::SellBase;
        let amt = victim.amount as f64;
        let (orca_p, ray_p) = if victim.venue == "Orca" {
            (orca0.after_base_swap(&base, sell_base, amt), ray0.clone())
        } else {
            (orca0.clone(), ray0.after_base_swap(&base, sell_base, amt))
        };
        // Exact optimal arb over the predicted state (borrow capped by config).
        let (size_raw, profit_raw, buy_orca) = optimal_arb(&orca_p, &ray_p, &base, c.borrow_usdc * 1e6);
        // Convert USDC profit → SOL lamports via the independent SOL/USDC price
        // (NOT the trading pool's price — wrong denominator when base ≠ SOL).
        let sol_price = *sol_usd.read().unwrap(); // USDC per SOL
        let profit_lamports = if sol_price > 0.0 { profit_raw / 1e6 / sol_price * 1e9 } else { 0.0 };

        // GATE: fire only genuinely profitable arbs (clears tip + fees).
        let fire = profit_lamports > c.min_profit_lamports as f64 && size_raw > 1_000_000.0;
        let _ = log_tx.send(LogMsg::Decision(DecisionLog {
            t: now(), venue: trigger.venue, slot: trigger.slot,
            fired: fire && !dry_run, reason: if fire { "profitable" } else { "below_threshold" },
        }));
        if !fire { continue; }

        let dir = if buy_orca { "orca→ray" } else { "ray→orca" };
        // Tip ≤ 50% of profit (leaves margin). The repay_buffer forces leg2 to
        // yield borrow + tip + fees in USDC, so a landed trade is net-positive
        // even if the prediction is optimistic; too-small gaps revert for free.
        let tip = (profit_lamports * (c.tip_fraction_bps as f64 / 1e4)).clamp(SENDER_MIN_TIP, profit_lamports * 0.8) as u64;
        const FEE_LAMPORTS: f64 = 20_000.0; // tx + priority + cushion
        let repay_buffer = if sol_price > 0.0 {
            ((tip as f64 + FEE_LAMPORTS) / 1e9 * sol_price * 1e6 * 1.05) as u64
        } else { 0 };
        let borrow_amount = size_raw as u64;

        // Dedup: the same victim tx can arrive multiple times (retransmits);
        // fire it at most once (a duplicate bundle would fail anyway).
        if !seen_sigs.insert(trigger.sig.clone()) { continue; }
        if seen_sigs.len() > 5000 { seen_sigs.clear(); }

        if dry_run {
            eprintln!("[dry] would co-bundle {dir} borrow={:.1}USDC profit={:.6}SOL tip={} buffer={:.3}USDC (victim {} {} {:.1})",
                borrow_amount as f64 / 1e6, profit_lamports / 1e9, tip, repay_buffer as f64 / 1e6, victim.venue, if sell_base { "sellBase" } else { "buyBase" }, amt);
            continue;
        }

        // Daily tip cap: CHECK only (don't pre-charge). Tips are paid only on a
        // landed bundle, so we count actual spend after acceptance, not per
        // attempt — otherwise non-landing fires falsely exhaust the cap.
        if *daily_tip_sol.read().unwrap() + tip as f64 / 1e9 > max_daily_tip_sol {
            alert(&webhook, "daily_cap", "daily tip cap reached");
            continue;
        }
        // Pace submissions (PACE_MS; Sender lifts the Jito 1/sec cap so this can
        // be small). Skip if we submitted too recently.
        if last_submit.elapsed() < Duration::from_millis(pace_ms) { continue; }
        let Some(ref kp) = kp else { continue };
        let Ok(mut tx) = build_arb_tx(pd, signer, &alt, borrow_amount, buy_orca, tip_account, tip, c.priority_micro_lamports, bh, repay_buffer) else { continue };
        drop(pd_guard);

        tx.signatures[0] = kp.sign_message(&tx.message.serialize());
        let sig = tx.signatures[0].to_string();
        let arb_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());

        fired += 1;
        eprintln!("[debug] BACKRUN {dir} borrow={:.1}USDC profit={:.6}SOL tip={} slot={} sig={}",
            borrow_amount as f64 / 1e6, profit_lamports / 1e9, tip, trigger.slot, &sig[..16.min(sig.len())]);
        // Submit our arb ALONE (not [victim, arb]): the victim is already
        // propagating to land via its own path (shred = already broadcasting),
        // so bundling it → "already processed". The victim's landing creates the
        // gap on-chain; our guarded arb bundle races to capture it. Guard reverts
        // free if the gap is already gone.
        last_submit = Instant::now();
        match send_sender(&sender_url, &arb_b64) {
            Ok(returned_sig) => {
                let _ = log_tx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: borrow_amount as f64 / 1e6, tip_lamports: tip, bundle_id: None, signature: Some(sig.clone()), bundle_status: None, realized_usdc: None, error: None }));
                eprintln!("⚡ backrun {dir} sent {}", &returned_sig[..16.min(returned_sig.len())]);
                let (ep, ltx, owner, s, borrow_ui, dtip) = (endpoint.clone(), log_tx.clone(), signer.to_string(), sig.clone(), borrow_amount as f64 / 1e6, daily_tip_sol.clone());
                std::thread::spawn(move || {
                    // Landing truth = the tx on-chain (getTransaction via realized_usdc);
                    // Sender returns a signature, not a Jito bundle id, so poll the chain.
                    let mut pnl = None;
                    for delay in [4u64, 8, 20] {
                        std::thread::sleep(Duration::from_secs(delay));
                        pnl = realized_usdc(&ep, &s, &owner);
                        if pnl.is_some() { break; }
                    }
                    // Count the tip against the daily cap ONLY on a confirmed landing
                    // (accepted-but-dropped pays no tip).
                    if pnl.is_some() { *dtip.write().unwrap() += tip as f64 / 1e9; }
                    eprintln!("[readback] {}… landed={} realized_usdc={:?}", &s[..8.min(s.len())], pnl.is_some(), pnl);
                    let _ = ltx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: borrow_ui, tip_lamports: tip, bundle_id: None, signature: Some(s), bundle_status: None, realized_usdc: pnl, error: None }));
                });
            }
            Err(e) => {
                let err_str = e.to_string();
                eprintln!("[debug] submit error ({dir}): {}", &err_str[..400.min(err_str.len())]);
                let _ = log_tx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: borrow_amount as f64 / 1e6, tip_lamports: tip, bundle_id: None, signature: None, bundle_status: None, realized_usdc: None, error: Some(err_str) }));
            }
        }

        if triggers % 100 == 0 { eprintln!("[executor] triggers={triggers} fired={fired}"); }
    }
}
