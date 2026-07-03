//! The arb executor (v1): shred trigger → build guarded arb (both directions)
//! → simulate → sign → submit Jito bundle → observe. DRY_RUN=1 (default)
//! evaluates + logs but never signs or submits.
//!
//! Hot-path discipline: build + sign are the only latency-sensitive steps;
//! all logging happens AFTER submit; realized P&L is read on a later poll.
//! v1 simulates before submitting (cheap insurance; the guard already makes a
//! bad tx revert for free). Optimizations noted inline for a later pass:
//! account-subscription instead of per-trigger pool fetches, blind submit,
//! tick-array refresh.
//!
//! Env: KEYPAIR_PATH, RPC_ENDPOINT, ALT_ADDRESS, SHREDSTREAM_PORT, BORROW_USDC,
//!      TIP_LAMPORTS, PRIORITY_MICRO_LAMPORTS, RUN_DIR, DRY_RUN,
//!      JITO_BLOCK_ENGINE, WALLET_MIN_SOL, MAX_DAILY_TIP_SOL, ALERT_WEBHOOK.

use arb_engine::arb::{build_arb_tx, load_alt, PoolData};
use arb_engine::jito::{default_block_engine, get_tip_accounts, send_bundle};
use arb_engine::observe::{alert, log_decision, log_trade, realized_usdc};
use arb_engine::pools::pair;
use base64::Engine;
use serde::Serialize;
use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use std::str::FromStr;
use std::sync::mpsc;
use std::time::{Duration, Instant};

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() {
                return Some(v);
            }
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

fn sol_balance(endpoint: &str, pubkey: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[pubkey]}))
        .and_then(|v| v["result"]["value"].as_f64())
        .unwrap_or(0.0)
        / 1e9
}

/// simulateTransaction of a signed/serialized v0 tx; returns err (None = clean).
fn simulate(endpoint: &str, tx_b64: &str) -> Option<serde_json::Value> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[tx_b64,{"encoding":"base64","sigVerify":false,"replaceRecentBlockhash":true}]}))?;
    Some(v["result"]["value"]["err"].clone())
}

#[derive(Serialize)]
struct Decision<'a> {
    t: String,
    pair: &'a str,
    venue: &'a str,
    slot: u64,
    dir: &'a str,
    sim_ok: bool,
    fired: bool,
    reason: &'a str,
}

#[derive(Serialize)]
struct Trade {
    t: String,
    pair: String,
    dir: &'static str,
    borrow_usdc: f64,
    tip_lamports: u64,
    bundle_id: Option<String>,
    signature: Option<String>,
    realized_usdc: Option<f64>,
    error: Option<String>,
}

fn now() -> String {
    // Wall-clock via the system; only used for log labels.
    use std::time::{SystemTime, UNIX_EPOCH};
    let s = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs();
    format!("{s}")
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let alt_addr = std::env::var("ALT_ADDRESS").expect("ALT_ADDRESS");
    let port: u16 = std::env::var("SHREDSTREAM_PORT").ok().and_then(|s| s.parse().ok()).unwrap_or(20000);
    let borrow_ui: f64 = std::env::var("BORROW_USDC").ok().and_then(|s| s.parse().ok()).unwrap_or(500.0);
    let borrow_amount = (borrow_ui * 1e6) as u64;
    let tip_lamports: u64 = std::env::var("TIP_LAMPORTS").ok().and_then(|s| s.parse().ok()).unwrap_or(10_000);
    let priority: u64 = std::env::var("PRIORITY_MICRO_LAMPORTS").ok().and_then(|s| s.parse().ok()).unwrap_or(10_000);
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".to_string());
    let dry_run = std::env::var("DRY_RUN").map(|v| v != "0").unwrap_or(true);
    let block_engine = default_block_engine();
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let webhook = std::env::var("ALERT_WEBHOOK").ok();

    let kp = std::env::var("KEYPAIR_PATH").ok().map(|p| {
        let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&p).expect("read keypair")).expect("parse keypair");
        Keypair::try_from(&bytes[..]).expect("keypair")
    });
    if kp.is_none() && !dry_run {
        panic!("LIVE mode needs KEYPAIR_PATH");
    }
    let signer = kp.as_ref().map(|k| k.pubkey()).unwrap_or_else(|| Pubkey::from_str("Anu6Awu4kxaEDrg1nkpcikx6tJ2xhfVci5TvDrZBsZEB").unwrap());
    let cfg = pair();

    // ALT + tip accounts (fetched once; ALT is stable, tips rotate rarely).
    let alt = load_alt(&alt_addr, &account_data(&endpoint, &alt_addr).expect("fetch ALT"));
    let tip_accounts = get_tip_accounts(&block_engine).unwrap_or_default();

    eprintln!(
        "executor v1 {} pair={} borrow=${} tip={}lam dir=both dry_run={} alt={} wallet={}",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" },
        cfg.label, borrow_ui, tip_lamports, dry_run, &alt_addr[..8], signer
    );
    if !dry_run {
        let bal = sol_balance(&endpoint, &signer.to_string());
        eprintln!("wallet balance: {bal} SOL");
        if bal < wallet_min_sol {
            panic!("wallet below floor {wallet_min_sol} SOL");
        }
    }

    // Shred trigger feed emits on a tokio channel; bridge it to a std channel
    // so the main loop is plain blocking iteration.
    let (tx, rx) = mpsc::channel();
    let (feed_tx, mut feed_rx) = tokio::sync::mpsc::unbounded_channel();
    let _feed = arb_engine::shredstream::run_shredstream_feed(port, Some(endpoint.clone()), feed_tx);
    std::thread::spawn(move || {
        // A tiny tokio runtime just to drain the feed into the std channel.
        let rt = tokio::runtime::Builder::new_current_thread().enable_all().build().unwrap();
        rt.block_on(async move {
            while let Some(trigger) = feed_rx.recv().await {
                if tx.send(trigger).is_err() {
                    break;
                }
            }
        });
    });

    let mut blockhash = latest_blockhash(&endpoint).unwrap_or_default();
    let mut last_bh = Instant::now();
    let mut daily_tip_sol = 0.0f64;
    let (mut triggers, mut fired, mut landed) = (0u64, 0u64, 0u64);

    for trigger in rx {
        triggers += 1;
        // Refresh blockhash every ~20s (valid ~60-90s).
        if last_bh.elapsed().as_secs() >= 20 {
            if let Some(bh) = latest_blockhash(&endpoint) {
                blockhash = bh;
            }
            last_bh = Instant::now();
        }

        // Fetch both pools fresh for this attempt. (v1: per-trigger RPC; later,
        // stream pool accounts to drop this latency.)
        let (Some(orca), Some(ray)) = (account_data(&endpoint, &cfg.orca_pool), account_data(&endpoint, &cfg.ray_pool)) else {
            continue;
        };
        let pools = PoolData { orca, ray };

        // Try both directions; submit the one that simulates clean (= the
        // exact-out guard was satisfied, i.e. a profitable round trip exists).
        for orca_first in [true, false] {
            let dir = if orca_first { "orca->ray" } else { "ray->orca" };
            let tip_to = tip_accounts.first().copied();
            let built = build_arb_tx(&pools, signer, &alt, borrow_amount, orca_first, tip_to, tip_lamports, priority, blockhash);
            let Ok(tx) = built else { continue };
            let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
            let sim_err = simulate(&endpoint, &b64);
            let sim_ok = matches!(sim_err, Some(ref e) if e.is_null());

            log_decision(&run_dir, &Decision {
                t: now(), pair: &cfg.label, venue: trigger.venue, slot: trigger.slot,
                dir, sim_ok, fired: sim_ok && !dry_run,
                reason: if sim_ok { "profitable" } else { "guard_reverts" },
            });

            if !sim_ok {
                continue; // no profitable round trip this direction
            }
            eprintln!("⚡ profitable {dir} slot {} — {}", trigger.slot, if dry_run { "DRY RUN" } else { "firing" });

            if dry_run {
                break;
            }
            // Daily tip cap + wallet floor.
            if daily_tip_sol + tip_lamports as f64 / 1e9 > max_daily_tip_sol {
                alert(&webhook, "daily_cap", "daily tip cap reached");
                break;
            }
            let Some(ref kp) = kp else { break };

            // Sign the v0 message and submit the bundle.
            let mut signed = tx;
            let msg_bytes = signed.message.serialize();
            signed.signatures[0] = kp.sign_message(&msg_bytes);
            let signed_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&signed).unwrap());
            let sig = signed.signatures[0].to_string();

            fired += 1;
            daily_tip_sol += tip_lamports as f64 / 1e9;
            match send_bundle(&block_engine, &[signed_b64]) {
                Ok(bundle_id) => {
                    log_trade(&run_dir, &Trade {
                        t: now(), pair: cfg.label.clone(), dir, borrow_usdc: borrow_ui,
                        tip_lamports, bundle_id: Some(bundle_id.clone()), signature: Some(sig.clone()),
                        realized_usdc: None, error: None,
                    });
                    eprintln!("   bundle {bundle_id} sig {}…", &sig[..16.min(sig.len())]);
                    // Realized P&L on a later poll (off hot path).
                    let (ep, rd, owner, s) = (endpoint.clone(), run_dir.clone(), signer.to_string(), sig.clone());
                    let pl = cfg.label.clone();
                    std::thread::spawn(move || {
                        std::thread::sleep(Duration::from_secs(10));
                        if let Some(pnl) = realized_usdc(&ep, &s, &owner) {
                            log_trade(&rd, &Trade {
                                t: now(), pair: pl, dir, borrow_usdc: borrow_ui, tip_lamports,
                                bundle_id: None, signature: Some(s), realized_usdc: Some(pnl), error: None,
                            });
                        }
                    });
                }
                Err(e) => {
                    log_trade(&run_dir, &Trade {
                        t: now(), pair: cfg.label.clone(), dir, borrow_usdc: borrow_ui, tip_lamports,
                        bundle_id: None, signature: None, realized_usdc: None, error: Some(e.to_string()),
                    });
                }
            }
            break; // one direction per trigger
        }

        if triggers % 50 == 0 {
            eprintln!("[executor] triggers={triggers} fired={fired} landed={landed}");
        }
        let _ = &mut landed;
    }
}
