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
use arb_engine::jito::{default_block_engine, get_tip_accounts, send_bundle};
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
    #[serde(default = "d_tip")]
    tip_lamports: u64,
    #[serde(default = "d_priority")]
    priority_micro_lamports: u64,
}
fn d_borrow() -> f64 { 500.0 }
fn d_tip() -> u64 { 10_000 }
fn d_priority() -> u64 { 10_000 }
impl Default for Config {
    fn default() -> Self {
        Self { paused: false, borrow_usdc: d_borrow(), tip_lamports: d_tip(), priority_micro_lamports: d_priority() }
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

    // Seed pool data + blockhash before starting.
    if let (Some(o), Some(r)) = (account_data(&endpoint, &cfg.orca_pool), account_data(&endpoint, &cfg.ray_pool)) {
        *pooldata.write().unwrap() = Some(PoolData { orca: o, ray: r });
    }
    if let Some(bh) = latest_blockhash(&endpoint) { *blockhash.write().unwrap() = bh; }

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
        let (ep, pd, bh) = (endpoint.clone(), pooldata.clone(), blockhash.clone());
        let fb = std::env::var("RPC_FALLBACK")
            .unwrap_or_else(|_| "https://api.mainnet-beta.solana.com".into());
        let (op, rp) = (cfg.orca_pool.clone(), cfg.ray_pool.clone());
        std::thread::spawn(move || {
            let mut pool_tick = 0u64;
            let mut bh_fails = 0u64;
            loop {
                std::thread::sleep(Duration::from_secs(3));
                match latest_blockhash(&ep).or_else(|| latest_blockhash(&fb)) {
                    Some(h) => {
                        bh_fails = 0;
                        *bh.write().unwrap() = h;
                    }
                    None => {
                        bh_fails += 1;
                        eprintln!("[warn] blockhash refresh failed on BOTH endpoints ({bh_fails} in a row) — cached hash going stale");
                    }
                }
                pool_tick += 1;
                if pool_tick % 4 == 0 {  // refresh pools every 12s (3s * 4)
                    let (o, r) = (
                        account_data(&ep, &op).or_else(|| account_data(&fb, &op)),
                        account_data(&ep, &rp).or_else(|| account_data(&fb, &rp)),
                    );
                    if let (Some(o), Some(r)) = (o, r) {
                        *pd.write().unwrap() = Some(PoolData { orca: o, ray: r });
                    } else {
                        eprintln!("[warn] pool data refresh failed on both endpoints");
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
    let last_submit = Arc::new(RwLock::new(Instant::now()));
    let (mut triggers, mut fired) = (0u64, 0u64);

    // ═══ HOT PATH ═══ memory reads + sign + submit only.
    for trigger in trig_rx {
        triggers += 1;
        let c = config.read().unwrap().clone();
        if c.paused { continue; }

        let should_fire = true; // blind fire every trigger (guard decides profitability)
        let _ = log_tx.send(LogMsg::Decision(DecisionLog {
            t: now(), venue: trigger.venue, slot: trigger.slot,
            fired: should_fire && !dry_run, reason: "blind_guarded",
        }));
        if !should_fire { continue; }

        if dry_run { continue; }

        // ── THROTTLE: 10/min (1 fire per 6 seconds) to avoid Jito rate-limiting ──
        // Check is O(1), nanoseconds — no latency impact on 1ms reaction.
        let now_instant = Instant::now();
        if now_instant.duration_since(*last_submit.read().unwrap()).as_secs() < 6 {
            continue;  // skip this trigger, keep listening for next opportunity
        }
        // Update throttle timer immediately (before any potential failure) to prevent
        // multiple submissions at the same slot if send_bundle fails.
        *last_submit.write().unwrap() = now_instant;

        // Build from CACHED state — no RPC here.
        let bh = *blockhash.read().unwrap();
        let borrow_amount = (c.borrow_usdc * 1e6) as u64;
        let pd_guard = pooldata.read().unwrap();
        let Some(ref pd) = *pd_guard else { continue };
        // Try both directions; submit whichever builds successfully (doesn't matter which).
        let (built_0, built_1) = (
            build_arb_tx(pd, signer, &alt, borrow_amount, false, tip_account, c.tip_lamports, c.priority_micro_lamports, bh),
            build_arb_tx(pd, signer, &alt, borrow_amount, true, tip_account, c.tip_lamports, c.priority_micro_lamports, bh),
        );
        drop(pd_guard);
        let Ok(mut tx) = built_0.or(built_1) else { continue };

        { // daily tip cap
            let mut d = daily_tip_sol.write().unwrap();
            if *d + c.tip_lamports as f64 / 1e9 > max_daily_tip_sol {
                alert(&webhook, "daily_cap", "daily tip cap reached");
                continue;
            }
            *d += c.tip_lamports as f64 / 1e9;
        }
        let Some(ref kp) = kp else { continue };
        let msg_bytes = tx.message.serialize();
        tx.signatures[0] = kp.sign_message(&msg_bytes);
        let sig = tx.signatures[0].to_string();
        let signed_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());

        fired += 1;
        let bh_str = bh.to_string();
        eprintln!("[debug] tx_size={} sig={} slot={} bh={}", bincode::serialize(&tx).unwrap().len(), &sig[..16.min(sig.len())], trigger.slot, &bh_str[..8.min(bh_str.len())]);
        match send_bundle(&block_engine, &[signed_b64.clone()]) {
            Ok(bundle_id) => {
                let _ = log_tx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: c.borrow_usdc, tip_lamports: c.tip_lamports, bundle_id: Some(bundle_id.clone()), signature: Some(sig.clone()), bundle_status: None, realized_usdc: None, error: None }));
                eprintln!("⚡ fired bundle {bundle_id}");
                // bundle status + realized P&L readback later, off hot path
                let (ep, be, ltx, owner, s, bid) = (endpoint.clone(), block_engine.clone(), log_tx.clone(), signer.to_string(), sig.clone(), bundle_id.clone());
                std::thread::spawn(move || {
                    // Poll early and late: catch the status while Jito still
                    // tracks the bundle (early), and the final verdict (late).
                    let mut statuses = Vec::new();
                    for delay in [3u64, 9, 30] {
                        std::thread::sleep(Duration::from_secs(delay));
                        statuses.push(arb_engine::jito::bundle_status(&be, &bid).unwrap_or_else(|| "unknown".into()));
                    }
                    let pnl = realized_usdc(&ep, &s, &owner);
                    eprintln!("[readback] bundle {}… status@3s/12s/42s={} realized_usdc={:?}", &bid[..8.min(bid.len())], statuses.join("/"), pnl);
                    let _ = ltx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: c.borrow_usdc, tip_lamports: c.tip_lamports, bundle_id: Some(bid), signature: Some(s), bundle_status: Some(statuses.join("/")), realized_usdc: pnl, error: None }));
                });
            }
            Err(e) => {
                let err_str = e.to_string();
                eprintln!("[debug] submit error: {}", &err_str[..400.min(err_str.len())]);
                let _ = log_tx.send(LogMsg::Trade(TradeLog { t: now(), borrow_usdc: c.borrow_usdc, tip_lamports: c.tip_lamports, bundle_id: None, signature: None, bundle_status: None, realized_usdc: None, error: Some(err_str) }));
            }
        }

        if triggers % 100 == 0 { eprintln!("[executor] triggers={triggers} fired={fired}"); }
    }
}
