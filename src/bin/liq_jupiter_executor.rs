//! Jupiter Lend (Fluid) liquidation executor — event-driven off Pyth Lazer,
//! DRY_RUN by default.
//!
//! Architecture (matches the marginfi executor src/bin/liq_executor.rs and the
//! Save rewrite): the TRIGGER is a Pyth Lazer WS tick, NOT a getProgramAccounts
//! poll. Vault STRUCTURE is refreshed off-band on a slow timer; the price-cross
//! recompute runs in-memory on every ms Lazer tick. `detect_lag` (now_us −
//! freshest Lazer publish ts) is logged in the heartbeat, and a per-detect
//! latency record is appended to {RUN_DIR}/latency.jsonl.
//!
//! STATUS: `try_arm` now derives the FULL liquidate account set PURELY FROM SEEDS
//! + on-chain state (`jupiter_fire::derive_liquidate_accounts` +
//! `jupiter::decode_oracle_sources`) — the old "lift the Liquidity PDAs from a
//! recent liquidate tx" dependency is GONE, so ANY in-scope vault resolves,
//! including ones that have never been liquidated. `col_per_unit_debt=0` accepts
//! the oracle price (a slippage floor, not the price) and `remaining_accounts`
//! come from `build_remaining_accounts`. DRY_RUN by default.
//!
//! FIRING IS LIVE (DRY_RUN=0): the hot path mirrors the Kamino executor's
//! submit-only branch — stamp a fresh blockhash onto the cached tx, sign with
//! KEYPAIR_PATH, and submit via Helius Sender (`jito::send_sender`). Money guards:
//! only ≤1232B + sim-clean armed txs are ever cached; DRY_RUN never submits; the
//! MAX_DAILY_TIP_SOL daily cap and WALLET_MIN_SOL floor gate every live send; a
//! per-vault HANDLE_COOLDOWN_SECS stops resubmitting a standing cross.
//!
//! HONEST FIRING GATE: `try_arm` arms only when the flash-loan-wrapped fire tx is
//! BOTH (a) ≤ 1232 bytes (submittable — needs JUP_ALT deployed, see `jup_alt_print`;
//! without it the wrap is ~1.5-1.7KB and is skip-and-logged, never armed) AND
//! (b) SIMULATES CLEAN. Until JUP_ALT is deployed on the box, arming is size-gated
//! off for USDC vaults — the account DERIVATION is proven (jupiter_seed_probe
//! PROOF A 159/159; jupiter_fire_probe STAGE 5 seed-derived liquidate gates at
//! VaultInvalidLiquidation 6027 on a no-recent-tx vault), the last step is the ALT.
//!
//! The per-tick recompute surfaces the CONFIDENT signal (vaults holding
//! absorbed/pending liquidation debt with in-scope debt). Tick band for account
//! selection = topmost down to `topmost-1` (includes the topmost tick; the program
//! walks/gates the rest) — a live vault-oracle→price mapping would tighten this.
//!
//! ARM + PRE-SIGN (fleet parity, src/bin/liq_executor.rs PR #45): the arm-cache,
//! off-band re-arm phase, and submit-only hot path are WIRED (keyed by vault_id).
//!
//! Scope: only vaults whose debt (borrow_token) is USDC/USDT/wSOL are armed
//! (via `VaultConfig::debt_in_scope`); the decoder/detection stay general.
//!
//! Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> JUP_ALT=<alt> LIQUIDATOR_MA=<ma>
//!        [DRY_RUN=1] [KEYPAIR_PATH=~/arb-keypair.json] [AUTHORITY=<pk>]
//!        [MAX_DAILY_TIP_SOL=0.05] [WALLET_MIN_SOL=0.02] [MIN_TIP_SOL=0.0002]
//!        [SENDER_URL=…] [SENDER_TIP_ACCOUNT=…] [HANDLE_COOLDOWN_SECS=20]
//!        [RUN_DIR=.] [TICK_POLL_MS=1] [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//!        cargo run --release --bin liq_jupiter_executor

use arb_engine::jito::send_sender;
use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
use arb_engine::lazer;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use solana_transaction::versioned::VersionedTransaction;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::sync::{Arc, Mutex};
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

fn now_us() -> u128 { SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_micros() }

/// Append a latency record to {run_dir}/latency.jsonl (same shape as the
/// marginfi executor: an event with detect-side timestamps).
fn log_latency(run_dir: &str, v: &serde_json::Value) {
    use std::io::Write;
    if let Ok(mut f) = std::fs::OpenOptions::new().create(true).append(true).open(format!("{run_dir}/latency.jsonl")) {
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
fn gpa_by_disc(endpoint: &str, disc: &[u8; 8]) -> Vec<(Pubkey, Vec<u8>)> {
    let disc58 = bs58::encode(disc).into_string();
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[jupiter::VAULTS_PROGRAM, {"encoding":"base64",
            "filters":[{"memcmp":{"offset":0,"bytes":disc58}}]}]}));
    let mut out = Vec::new();
    for e in v.as_ref().and_then(|v| v["result"].as_array()).into_iter().flatten() {
        if let (Some(pk), Some(data)) = (
            e["pubkey"].as_str().and_then(|s| s.parse::<Pubkey>().ok()),
            b64(&e["account"]["data"]),
        ) { out.push((pk, data)); }
    }
    out
}

/// Off-band vault STRUCTURE refresh (not the trigger): load + join all vaults.
fn load_vaults(endpoint: &str) -> Vec<Vault> {
    let mut configs: HashMap<u16, (Pubkey, VaultConfig)> = HashMap::new();
    for (pk, d) in gpa_by_disc(endpoint, &jupiter::VAULT_CONFIG_DISC) {
        if let Some(c) = VaultConfig::decode(&d) { configs.insert(c.vault_id, (pk, c)); }
    }
    let mut states: HashMap<u16, (Pubkey, VaultState)> = HashMap::new();
    for (pk, d) in gpa_by_disc(endpoint, &jupiter::VAULT_STATE_DISC) {
        if let Some(s) = VaultState::decode(&d) { states.insert(s.vault_id, (pk, s)); }
    }
    let mut vaults = Vec::new();
    for (vid, (cpk, c)) in &configs {
        if let Some((spk, s)) = states.get(vid) {
            vaults.push(Vault { config_pubkey: *cpk, state_pubkey: *spk, config: c.clone(), state: s.clone() });
        }
    }
    vaults.sort_by_key(|v| v.config.vault_id);
    vaults
}

/// Lazer feed id for a vault's collateral mint (falls back to the debt mint),
/// so the detection hook has the price this vault liquidates against.
fn feed_for_vault(v: &Vault, feed_map: &HashMap<Pubkey, u32>) -> Option<u32> {
    feed_map.get(&v.config.supply_token).or_else(|| feed_map.get(&v.config.borrow_token)).copied()
}

/// Read raw account bytes (used by the seed-derivation arm path: oracle decode,
/// mint-owner lookup, and `build_remaining_accounts`' PDA existence probes).
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
}

/// Fresh blockhash for stamping the pre-built fire tx at submit time.
fn latest_blockhash(endpoint: &str) -> Option<solana_hash::Hash> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash",
        "params":[{"commitment":"finalized"}]}))?;
    solana_hash::Hash::from_str(v["result"]["value"]["blockhash"].as_str()?).ok()
}

/// Wallet SOL balance (the WALLET_MIN_SOL floor guard), in SOL.
fn sol_balance(endpoint: &str, owner: &str) -> f64 {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getBalance","params":[owner]}))
        .and_then(|v| v["result"]["value"].as_u64()).map(|l| l as f64 / 1e9).unwrap_or(0.0)
}

/// A pre-built, sim-gated liquidate tx for one vault, ready to submit the instant
/// its tick crosses. Same role as the Kamino executor's `CachedFire`: the hot path
/// stamps a fresh blockhash, signs with the keypair, and submits — NO build/quote/
/// sim on the critical path. Compiled with a placeholder blockhash (stamped at
/// fire) and a placeholder signature (filled at fire).
struct Armed {
    /// The sim-gated, ≤1232B fire tx (mirrors Kamino's CachedFire.tx).
    tx: VersionedTransaction,
    /// Serialized byte length (already ≤1232 — the arm size gate guarantees it).
    tx_bytes: usize,
    /// Jupiter-quoted debt-asset out for the seized-collateral swap leg.
    quoted_out: u64,
    /// Tip baked into the cached tx (for the daily-cap accounting at fire time).
    tip_lamports: u64,
    tip_sol: f64,
    /// now_us() when built (for staleness-based re-arm).
    built_us: u128,
}

/// Off-band ARM step: build + quote + sim the flash-loan liquidate tx for a vault
/// near its liquidation boundary, so the crossing tick submits only.
///
/// SEED-DERIVED (no captured tx). The full liquidate account set — the Fluid
/// Liquidity-program PDAs (reserves/positions/rate models/token accounts/liquidity),
/// `new_branch`, and the oracle `sources` — is derived PURELY from seeds +
/// on-chain vault/oracle state via `jupiter_fire::derive_liquidate_accounts` +
/// `jupiter::decode_oracle_sources`. This is what lets ANY in-scope vault arm,
/// including ones with no recent liquidate tx (validated: jupiter_seed_probe
/// PROOF A = 159/159; jupiter_fire_probe STAGE 5 = seed-derived set gates at
/// VaultInvalidLiquidation 6027 on a no-recent-tx vault).
///
/// `col_per_unit_debt` = 0 accepts the oracle price (a slippage floor, not the
/// price; the program prices from its own oracle). `remaining_accounts` + indices
/// come from `build_remaining_accounts` (tick band = topmost down to `liq_tick`).
/// Scope: the flash-loan wrap is USDC-debt only (mirrors save_fire); returns None
/// otherwise. We arm only when the priced fire tx SIMULATES CLEAN (liquidatable
/// now AND fits a single packet — i.e. JUP_ALT is deployed); a gated/oversized tx
/// is NOT cached. Honest guard: if the oracle can't be decoded or the sim isn't
/// clean, we return None and log why — never pre-sign a mispriced/unfittable tx.
#[allow(clippy::too_many_arguments)]
fn try_arm(
    endpoint: &str, v: &Vault, authority: &Pubkey, liquidator_ma: &Pubkey,
    tip_account: Pubkey, tip_lamports: u64, tip_sol: f64,
) -> Option<Armed> {
    if v.config.debt_label() != "USDC" { return None; }
    // Oracle price sources straight from the vault's oracle account (in order).
    let sources = get_acct(endpoint, &v.config.oracle)
        .as_deref().and_then(jupiter::decode_oracle_sources)?;
    if sources.is_empty() { return None; }
    let collat_mint = v.config.supply_token;
    let ctp = get_acct_owner(endpoint, &collat_mint)
        .unwrap_or_else(|| "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA".parse().unwrap());
    let btp = get_acct_owner(endpoint, &v.config.borrow_token)
        .unwrap_or_else(|| "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA".parse().unwrap());
    // Tick band: topmost down to `liq_tick`. topmost-1 includes the topmost tick
    // (the only mandatory one); the program itself walks/gates the rest.
    let liq_tick = v.state.topmost_tick - 1;
    let fetch = |pk: &Pubkey| -> Option<Vec<u8>> { get_acct(endpoint, pk) };
    let (remaining, indices) = arb_engine::jupiter_fire::build_remaining_accounts(
        v.config.vault_id, v.state.topmost_tick, v.state.current_branch_id, liq_tick, &sources, &fetch);

    let mut fa = arb_engine::jupiter_fire::derive_liquidate_accounts(v, ctp, btp);
    fa.remaining = remaining.clone();
    // Size the repay by a fraction of the vault's total borrow (native units).
    let debt_amt = (v.state.total_borrow / 50).max(1_000_000);
    let seize = debt_amt.max(1); // nominal; the swap quote refines it
    let cand = arb_engine::jupiter_fire::JupiterFireCandidate {
        accts: fa, debt_amt, col_per_unit_debt: 0,
        remaining, remaining_indices: indices,
        seize_underlying: seize, collateral_mint: collat_mint, collateral_token_program: ctp,
    };
    // Build WITH the tip baked in (mirrors Kamino's try_arm) so the hot path is
    // pure submit — the tx we sim-gate is byte-identical to the tx we submit. Tip=0
    // (DRY_RUN default) simply omits the transfer ix. build_jupiter_fire_tx folds in
    // JUP_ALT/LIQ_ALT from env, so the wrapped fire drops ≤1232 once JUP_ALT is set.
    let tip = (tip_lamports > 0).then_some(tip_account);
    let fire = arb_engine::jupiter_fire::build_jupiter_fire_tx(
        endpoint, &cand, liquidator_ma, authority, tip, tip_lamports, 50_000, 100, 16, solana_hash::Hash::default(),
    ).ok()?;
    // Submittable-size gate (HONEST): `simulateTransaction` does NOT enforce the
    // 1232-byte single-packet limit, but `sendTransaction` does. Never cache a tx
    // we couldn't actually submit. JUP_ALT is folded in above (without it the wrap
    // is ~1.5-1.7KB); the remaining overflow on a tight vault is the MANDATORY
    // Helius Sender tip (~50-80B) plus this vault's per-state tick/branch remaining
    // accounts (not ALT-able, they vary per liquidation). Low-branch vaults fit;
    // high-branch ones size-gate off here — never armed, never sent.
    if fire.tx_bytes > 1232 {
        eprintln!("     · vault {} composes CLEAN but fire tx is {}B > 1232 (JUP_ALT applied; tip + {} branch \
            remaining accts exceed headroom) — size-gated off, not arming",
            v.config.vault_id, fire.tx_bytes, indices[1]);
        return None;
    }
    // Sim-gate: arm only on a clean sim (fireable now).
    use base64::Engine;
    let b64tx = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).ok()?);
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
    // A clean sim REQUIRES a present result.value with err == null. (Guard against
    // reading an RPC-level error object — no `result` — as "clean": serde Index on
    // a missing key yields Null, and Null.as_null() is Some, which would false-arm.)
    let clean = sim.as_ref()
        .and_then(|v| v["result"].get("value"))
        .map(|val| val["err"].is_null())
        .unwrap_or(false);
    if !clean { return None; }
    Some(Armed {
        tx: fire.tx, tx_bytes: fire.tx_bytes, quoted_out: fire.quoted_usdc_out,
        tip_lamports, tip_sol, built_us: now_us(),
    })
}

/// Fire an armed tx: stamp fresh blockhash, sign, submit via Helius Sender, log the
/// signature. Mirrors the Kamino executor's `fire_cached` submit-only branch — NO
/// build/quote/sim here. Money-code guards (in order): defensive ≤1232 re-check,
/// DRY_RUN never submits, MAX_DAILY_TIP_SOL daily cap, WALLET_MIN_SOL floor.
#[allow(clippy::too_many_arguments)]
fn fire_armed(
    endpoint: &str, run_dir: &str, sender_url: &str, dry_run: bool,
    vault_id: u16, armed: &Armed, authority: &Pubkey, fresh_bh: solana_hash::Hash,
    kp: Option<&Keypair>, daily_tip: &Arc<Mutex<f64>>, max_daily_tip: f64, wallet_min: f64,
) {
    let submit_us = now_us();
    let rec = |extra: serde_json::Value| {
        let mut j = serde_json::json!({
            "event": "fire", "protocol": "jupiter", "vault_id": vault_id,
            "quoted_out": armed.quoted_out, "armed_age_us": (submit_us - armed.built_us).to_string(),
            "submit_us": submit_us.to_string(), "tx_bytes": armed.tx_bytes,
            "tip_lamports": armed.tip_lamports,
        });
        if let (Some(o), Some(e)) = (j.as_object_mut(), extra.as_object()) {
            for (k, v) in e { o.insert(k.clone(), v.clone()); }
        }
        log_latency(run_dir, &j);
    };
    // Defensive: the arm-cache only holds ≤1232B, sim-clean txs — re-check size
    // before ever touching the wire (never submit an unsendable packet).
    if armed.tx_bytes > 1232 {
        eprintln!("[jup-exec] REFUSING vault {vault_id}: cached tx {}B > 1232", armed.tx_bytes);
        return;
    }
    if dry_run {
        rec(serde_json::json!({"dry_run": true, "fired": false}));
        println!("     ⓘ DRY_RUN: would FIRE vault {vault_id} ({}B, tip {:.5} SOL) — not submitting", armed.tx_bytes, armed.tip_sol);
        return;
    }
    // Daily tip cap + wallet floor — identical to the Kamino executor.
    if *daily_tip.lock().unwrap() + armed.tip_sol > max_daily_tip {
        eprintln!("[jup-exec] daily tip cap reached — not firing vault {vault_id}");
        rec(serde_json::json!({"dry_run": false, "fired": false, "error": "daily tip cap"}));
        return;
    }
    if sol_balance(endpoint, &authority.to_string()) < wallet_min {
        eprintln!("[jup-exec] wallet below floor {wallet_min} SOL — not firing vault {vault_id}");
        rec(serde_json::json!({"dry_run": false, "fired": false, "error": "wallet below floor"}));
        return;
    }
    let mut tx = armed.tx.clone();
    tx.message.set_recent_blockhash(fresh_bh);
    let kp = kp.expect("live fire requires KEYPAIR_PATH");
    tx.signatures[0] = kp.sign_message(&tx.message.serialize());
    let sig = tx.signatures[0].to_string();
    use base64::Engine;
    let tx_b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
    match send_sender(sender_url, &tx_b64) {
        Ok(_) => {
            *daily_tip.lock().unwrap() += armed.tip_sol;
            eprintln!("[jup-exec] FIRED {sig}");
            rec(serde_json::json!({"dry_run": false, "fired": true, "signature": sig}));
        }
        Err(e) => {
            eprintln!("[jup-exec] send failed: {e}");
            rec(serde_json::json!({"dry_run": false, "fired": false, "error": e.to_string()}));
        }
    }
}

fn get_acct_owner(endpoint: &str, pk: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    v["result"]["value"]["owner"].as_str()?.parse().ok()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| ".".into());
    let tick_poll_ms: u64 = std::env::var("TICK_POLL_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(1);
    let vault_refresh: u64 = std::env::var("VAULT_REFRESH_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30);
    let hb_every: u64 = std::env::var("HEARTBEAT_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(10);
    let dry_run = std::env::var("DRY_RUN").map(|v| v != "0").unwrap_or(true);

    // ── SUBMIT config (mirrors the Kamino executor) ──
    let sender_url = std::env::var("SENDER_URL").unwrap_or_else(|_| "http://ams-sender.helius-rpc.com/fast".into());
    let tip_account = Pubkey::from_str(&std::env::var("SENDER_TIP_ACCOUNT")
        .unwrap_or_else(|_| "2nyhqdwKcJZR2vcqCyrYsaPVdAnFoJjiksCXJ7hfEYgD".into())).unwrap();
    let min_tip_sol: f64 = std::env::var("MIN_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.0002);
    let max_daily_tip_sol: f64 = std::env::var("MAX_DAILY_TIP_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.05);
    let wallet_min_sol: f64 = std::env::var("WALLET_MIN_SOL").ok().and_then(|s| s.parse().ok()).unwrap_or(0.02);
    let handle_cooldown = Duration::from_secs(std::env::var("HANDLE_COOLDOWN_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(20));
    // Flat tip baked into the armed fire tx (Jupiter has no per-vault profit calc;
    // the tx's own fixed-payback guard is the profit-or-revert protection).
    let tip_sol = min_tip_sol;
    let tip_lamports = (tip_sol * 1e9) as u64;
    // The marginfi flash account for the flash-loan wrap. Defaults to the fleet
    // liquidator's account (same default as liq_executor) — the 2026-07-13 run
    // silently never armed because the env var was unset and there was no
    // fallback.
    let liquidator_ma: Option<Pubkey> = std::env::var("LIQUIDATOR_MA")
        .unwrap_or_else(|_| "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD".into())
        .parse().ok();

    // Keypair (submit-side): LIVE requires it; DRY_RUN falls back to AUTHORITY env
    // (or the fleet default) so arm/sim still exercise the real-wallet constraints.
    let kp = std::env::var("KEYPAIR_PATH").ok().and_then(|p| {
        std::fs::read_to_string(&p).ok()
            .and_then(|s| serde_json::from_str::<Vec<u8>>(&s).ok())
            .and_then(|b| Keypair::try_from(&b[..]).ok())
    });
    if kp.is_none() && !dry_run { panic!("LIVE fire needs KEYPAIR_PATH"); }
    let authority = kp.as_ref().map(|k| k.pubkey()).unwrap_or_else(|| {
        Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak".into())).unwrap()
    });
    let daily_tip = Arc::new(Mutex::new(0.0f64));
    let mut tip_day = now_us() / 86_400_000_000;
    let mut fresh_bh = solana_hash::Hash::default();
    let mut last_bh = Instant::now() - Duration::from_secs(9999);
    // Per-vault fire cooldown — don't resubmit the same standing cross every tick.
    let mut handled: HashMap<u16, Instant> = HashMap::new();

    // Event-driven trigger: Pyth Lazer WS, same feeds as the other executors.
    let lazer_table = arb_engine::pyth::new_table();
    let lazer_on = match std::env::var("PYTH_LAZER_TOKEN") {
        Ok(tok) if !tok.is_empty() => {
            lazer::spawn_lazer_thread(tok, lazer::arm_feed_ids(), lazer_table.clone());
            true
        }
        _ => { eprintln!("[jup-exec] no PYTH_LAZER_TOKEN — falling back to timed rescan (NOT event-driven)"); false }
    };
    let feed_map = lazer::mint_feed_map();

    println!("[jup-exec] Jupiter Lend (Fluid) executor {}  authority={authority} lazer={lazer_on}  (fire gated: ≤1232B + sim-clean; JUP_ALT required)",
        if dry_run { "[DRY RUN]" } else { "[LIVE]" });
    if !dry_run {
        let bal = sol_balance(&endpoint, &authority.to_string());
        eprintln!("[jup-exec] wallet balance: {bal} SOL");
        assert!(bal >= wallet_min_sol, "wallet below floor {wallet_min_sol}");
    }

    let mut vaults = load_vaults(&endpoint);
    println!("[jup-exec] loaded {} vaults; trigger = {}", vaults.len(),
        if lazer_on { "Pyth Lazer tick (event-driven)" } else { "timed rescan (fallback)" });

    let mut last_refresh = Instant::now();
    let mut last_hb = Instant::now();
    let mut last_tick_us: u64 = 0;
    let mut reported: HashSet<u16> = HashSet::new();
    // Arm-cache keyed by vault_id: pre-signed txs ready for submit-only firing.
    let mut arm_cache: HashMap<u16, Armed> = HashMap::new();

    loop {
        // Off-band STRUCTURE refresh — NOT the trigger.
        if last_refresh.elapsed() >= Duration::from_secs(vault_refresh) {
            vaults = load_vaults(&endpoint);
            last_refresh = Instant::now();
            reported.clear(); // re-report candidates against the fresh structure
        }

        // Reset the daily tip budget at the UTC-day boundary; refresh the fire
        // blockhash every ~2s so a crossing tick submits with a near-current hash.
        let day = now_us() / 86_400_000_000;
        if day != tip_day { tip_day = day; *daily_tip.lock().unwrap() = 0.0; }
        if !dry_run && last_bh.elapsed() >= Duration::from_secs(2) {
            if let Some(bh) = latest_blockhash(&endpoint) { fresh_bh = bh; last_bh = Instant::now(); }
        }

        // ── TRIGGER: block until a fresh Lazer tick (in-memory, no RPC) ──
        if lazer_on {
            let deadline = Instant::now() + Duration::from_secs(1);
            loop {
                let cur = lazer::arm_feed_ids().into_iter()
                    .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
                if cur > last_tick_us { last_tick_us = cur; break; }
                if Instant::now() >= deadline { break; }
                std::thread::sleep(Duration::from_millis(tick_poll_ms));
            }
        } else {
            std::thread::sleep(Duration::from_secs(vault_refresh.max(1)));
        }

        // Price snapshot for THIS tick.
        let snap: HashMap<u32, f64> = lazer::arm_feed_ids().into_iter()
            .filter_map(|f| Some((f, arb_engine::pyth::get(&lazer_table, f)?.price))).collect();

        // ── Detection on the tick (in-memory over the snapshot) ──
        // CONFIDENT signal today; the live price per vault is resolved and passed
        // to the hook so this becomes a true price-cross the moment tick↔price math lands.
        let cands: Vec<&Vault> = vaults.iter()
            .filter(|v| v.config.debt_in_scope() && v.maybe_liquidatable())
            .collect();

        // ── HOT PATH: submit-only for any crossing vault that is armed ──
        // Detect→submit ~0 when armed (blockhash-stamp + sign + send, no build/quote/
        // sim). Mirrors the Kamino executor's fire_cached branch. A per-vault
        // handle_cooldown stops resubmitting the same standing cross every tick.
        for v in &cands {
            let vid = v.config.vault_id;
            if handled.get(&vid).is_some_and(|t| t.elapsed() < handle_cooldown) { continue; }
            if let Some(a) = arm_cache.get(&vid) {
                handled.insert(vid, Instant::now());
                fire_armed(&endpoint, &run_dir, &sender_url, dry_run, vid, a, &authority,
                    fresh_bh, kp.as_ref(), &daily_tip, max_daily_tip_sol, wallet_min_sol);
            }
        }

        // Heartbeat with detect_lag (now_us − freshest Lazer publish).
        if hb_every > 0 && last_hb.elapsed() >= Duration::from_secs(hb_every) {
            let total = lazer::arm_feed_ids().len();
            let freshest = lazer::arm_feed_ids().into_iter()
                .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            let lag_ms = now_us().saturating_sub(freshest as u128) / 1000;
            eprintln!("[hb] lazer feeds {}/{} live | detect_lag {}ms | {} vaults | {} in-scope candidate(s) | {}",
                snap.len(), total, lag_ms, vaults.len(), cands.len(), lazer::status(&lazer_table));
            last_hb = Instant::now();
        }

        // Report/resolve each NEW candidate once per structure cycle (RPC off the
        // tick path). Emits a per-detect latency record (detect vs Lazer publish).
        for v in &cands {
            if !reported.insert(v.config.vault_id) { continue; }
            let c = &v.config;
            let feed = feed_for_vault(v, &feed_map);
            let price = feed.and_then(|f| snap.get(&f).copied());
            let freshest = lazer::arm_feed_ids().into_iter()
                .filter_map(|f| arb_engine::pyth::get(&lazer_table, f).map(|p| p.ts_us)).max().unwrap_or(0);
            let detect_lag_us = now_us().saturating_sub(freshest as u128);
            log_latency(&run_dir, &serde_json::json!({
                "event": "detect",
                "protocol": "jupiter",
                "vault_id": c.vault_id,
                "debt": c.debt_label(),
                "lazer_feed": feed,
                "lazer_price": price,
                "lazer_ts_us": freshest,
                "detect_us": now_us().to_string(),
                "detect_lag_us": detect_lag_us.to_string(),
                "absorbed_debt": v.state.absorbed_debt_amount.to_string(),
                "liq_threshold_bps": c.liquidation_threshold,
                "fired": false,
                "reason": "detection-only (col_per_unit_debt + remaining-accounts unsolved)"
            }));
            let collat = &c.supply_token.to_string()[..6];
            println!("  ▸ vault {} [{}→{}] LT {:.1}% absorbed_debt={} price={:?} detect_lag={}µs",
                c.vault_id, collat, c.debt_label(), c.liq_threshold_frac() * 100.0,
                v.state.absorbed_debt_amount, price, detect_lag_us);
            // Off-band ARM: derive the FULL account set from seeds (no captured tx
            // needed), build the priced flash-loan fire tx, and sim-gate it. Needs
            // LIQUIDATOR_MA (the marginfi flash account); skip arming without it.
            let armed = liquidator_ma.and_then(|lma|
                try_arm(&endpoint, v, &authority, &lma, tip_account, tip_lamports, tip_sol));
            match armed {
                Some(armed) => {
                    println!("     ✓ ARMED — seed-derived, priced fire tx simulates clean ({}B)", armed.tx_bytes);
                    arm_cache.insert(c.vault_id, armed);
                }
                None => println!("     · not armed{}: not fireable at the live price, non-USDC debt, \
                    or fire tx > 1232B (deploy JUP_ALT — see `cargo run --bin jup_alt_print`) — sim-gated, not sending",
                    if liquidator_ma.is_none() { " (LIQUIDATOR_MA unset/invalid — arming disabled)" } else { "" }),
            }
        }
    }
}
