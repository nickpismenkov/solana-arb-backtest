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
//! STATUS: the two Fluid pieces that used to block firing are now REVERSED
//! (src/jupiter_math.rs, verified against real txs by jupiter_fire_probe) —
//! `col_per_unit_debt` (a slippage floor in 1e15, not the price) and
//! `remaining_accounts_indices` + the tick/branch account selection. `try_arm`
//! now builds a correctly-priced, flash-loan-wrapped fire tx and sim-gates it.
//! DRY_RUN by default; still never submits from this loop.
//!
//! The per-tick recompute still surfaces the CONFIDENT signal (vaults holding
//! absorbed/pending liquidation debt with in-scope debt). The live Lazer price is
//! passed to the detection hook; a production price-cross trigger needs the
//! vault-oracle→price mapping (decimals/oracle semantics) pinned — hence arming
//! reconstructs `liquidation_tick` from a recent captured col_per_unit_debt (the
//! exact, probe-verified path) rather than the raw Lazer price.
//!
//! ARM + PRE-SIGN (fleet parity, src/bin/liq_executor.rs PR #45): the arm-cache,
//! off-band re-arm phase, and submit-only hot path are WIRED (keyed by vault_id).
//! `try_arm` builds `jupiter_fire::build_jupiter_fire_tx` (marginfi flashloan +
//! liquidate + Jupiter swap + repay) and arms only when it SIMULATES CLEAN — so
//! the cache holds only genuinely-fireable, correctly-priced txs.
//!
//! Scope: only vaults whose debt (borrow_token) is USDC/USDT/wSOL are armed
//! (via `VaultConfig::debt_in_scope`); the decoder/detection stay general.
//!
//! Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> [RUN_DIR=.] [TICK_POLL_MS=1]
//!        [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//!        cargo run --release --bin liq_jupiter_executor

use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
use arb_engine::jupiter_fire::{accounts_from_captured, LIQUIDATE_DISC};
use arb_engine::jupiter_math;
use arb_engine::lazer;
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
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

/// Derive-from-truth account resolver: find a recent liquidate tx that touched
/// this vault's config and lift its ordered account list (the vaults IDL has no
/// PDA seeds). Returns the liquidate ix's account pubkeys (26 + remaining), or
/// None if no recent liquidate references this vault_config.
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
}

/// Resolve a liquidate account set + oracle-source count + the captured
/// col_per_unit_debt (0 if that liquidator accepted the oracle price) from a
/// recent tx for this vault.
fn resolve_liquidate_accounts(endpoint: &str, vault_config: &Pubkey) -> Option<(Vec<Pubkey>, u8, u128)> {
    let sigs = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
        "params":[vault_config.to_string(), {"limit":200}]}))?;
    let arr = sigs["result"].as_array()?;
    let prog = jupiter::VAULTS_PROGRAM.parse::<Pubkey>().ok()?;
    for e in arr {
        if !e["err"].is_null() { continue; }
        let sig = e["signature"].as_str()?;
        let tx = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"json","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]}))?;
        let msg = &tx["result"]["transaction"]["message"];
        let mut all: Vec<Pubkey> = msg["accountKeys"].as_array()?.iter()
            .filter_map(|k| k.as_str().and_then(|s| s.parse().ok())).collect();
        if let Some(la) = tx["result"]["meta"]["loadedAddresses"].as_object() {
            for side in ["writable", "readonly"] {
                for k in la.get(side).and_then(|v| v.as_array()).into_iter().flatten() {
                    if let Some(pk) = k.as_str().and_then(|s| s.parse().ok()) { all.push(pk); }
                }
            }
        }
        let check = |ix: &serde_json::Value| -> Option<(Vec<Pubkey>, u8, u128)> {
            let pidx = ix["programIdIndex"].as_u64()? as usize;
            if all.get(pidx)? != &prog { return None; }
            let data = bs58::decode(ix["data"].as_str()?).into_vec().ok()?;
            if data.len() < 8 || data[..8] != LIQUIDATE_DISC { return None; }
            let col = u128::from_le_bytes(data.get(16..32)?.try_into().ok()?);
            // sources count = remaining_accounts_indices[0]; the Vec<u8> follows
            // debt_amt(8) col(16) absorb(1) transfer_type(1|2) len(4) at offset 8.
            let mut o = 8 + 8 + 16 + 1;
            o += if *data.get(o)? == 1 { 2 } else { 1 };
            let src = *data.get(o + 4)?; // first index byte
            let accts = ix["accounts"].as_array()?.iter()
                .filter_map(|i| i.as_u64().and_then(|i| all.get(i as usize)).copied()).collect();
            Some((accts, src, col))
        };
        for ix in msg["instructions"].as_array().into_iter().flatten() {
            if let Some(a) = check(ix) { return Some(a); }
        }
        for inner in tx["result"]["meta"]["innerInstructions"].as_array().into_iter().flatten() {
            for ix in inner["instructions"].as_array().into_iter().flatten() {
                if let Some(a) = check(ix) { return Some(a); }
            }
        }
    }
    None
}

/// A pre-built, pre-signed liquidate tx for one vault, ready to submit the
/// instant its tick crosses. Same role as the marginfi executor's armed cache.
#[allow(dead_code)]
struct Armed {
    /// Serialized, signed tx bytes — hot path does blockhash-stamp + submit only.
    tx: Vec<u8>,
    /// Jupiter-quoted debt-asset out for the seized-collateral swap leg.
    quoted_out: u64,
    /// now_us() when built (for staleness-based re-arm).
    built_us: u128,
}

/// Off-band ARM step: build + quote + sim the flash-loan liquidate tx for a vault
/// near its liquidation boundary, so the crossing tick submits only.
///
/// NOW WIRED (the two Fluid pieces are reversed — see src/jupiter_math.rs).
/// `col_per_unit_debt` = 0 accepts the oracle price (it is a slippage floor, not
/// the price; the program prices from its own oracle — proven safe by real txs).
/// `remaining_accounts` + indices are derived FRESH from live state via
/// `build_remaining_accounts`, with `liquidation_tick` reconstructed from a recent
/// captured col_per_unit_debt. Scope: the flash-loan wrap is USDC-debt only
/// (mirrors save_fire); returns None otherwise. We arm only when the priced fire
/// tx SIMULATES CLEAN (liquidatable now) — a gated/oversized tx is not cached.
fn try_arm(endpoint: &str, v: &Vault, accts: &[Pubkey], sources_count: u8, captured_col: u128) -> Option<Armed> {
    if v.config.debt_label() != "USDC" { return None; }
    let n = sources_count as usize;
    if accts.len() < 26 + n { return None; }
    let sources: Vec<Pubkey> = accts[26..26 + n].to_vec();
    // Reconstruct the liquidation tick from a recent captured col_per_unit_debt on
    // this vault (the exact, probe-verified path). Requires that liquidator to
    // have passed a non-zero floor; if they accepted the oracle price (0), we
    // can't reconstruct the band here and skip arming (honest: no false arm).
    let liq_tick = jupiter_math::liquidation_tick_from_col_per_debt(
        captured_col, v.config.liquidation_penalty, v.config.liquidation_threshold)?;
    let fetch = |pk: &Pubkey| -> Option<Vec<u8>> { get_acct(endpoint, pk) };
    let (remaining, indices) = arb_engine::jupiter_fire::build_remaining_accounts(
        v.config.vault_id, v.state.topmost_tick, v.state.current_branch_id, liq_tick, &sources, &fetch);

    let mut fa = accounts_from_captured(v, accts)?;
    fa.remaining = remaining.clone();
    let collat_mint = v.config.supply_token;
    let ctp = get_acct_owner(endpoint, &collat_mint)
        .unwrap_or_else(|| "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA".parse().unwrap());
    // Size the repay by a fraction of the vault's total borrow (native units).
    let debt_amt = (v.state.total_borrow / 50).max(1_000_000);
    let seize = debt_amt.max(1); // nominal; the swap quote refines it
    let cand = arb_engine::jupiter_fire::JupiterFireCandidate {
        accts: fa, debt_amt, col_per_unit_debt: 0,
        remaining, remaining_indices: indices,
        seize_underlying: seize, collateral_mint: collat_mint, collateral_token_program: ctp,
    };
    let authority: Pubkey = std::env::var("AUTHORITY").ok().and_then(|s| s.parse().ok())?;
    let liquidator_ma: Pubkey = std::env::var("LIQUIDATOR_MA").ok().and_then(|s| s.parse().ok())?;
    let fire = arb_engine::jupiter_fire::build_jupiter_fire_tx(
        endpoint, &cand, &liquidator_ma, &authority, None, 0, 50_000, 100, 16, solana_hash::Hash::default(),
    ).ok()?;
    // Sim-gate: arm only on a clean sim (fireable now).
    use base64::Engine;
    let b64tx = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&fire.tx).ok()?);
    let sim = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b64tx, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}));
    let clean = sim.as_ref().and_then(|v| v["result"]["value"]["err"].as_null()).is_some();
    if !clean { return None; }
    Some(Armed { tx: bincode::serialize(&fire.tx).ok()?, quoted_out: fire.quoted_usdc_out, built_us: now_us() })
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

    println!("[jup-exec] Jupiter Lend (Fluid) executor — DRY_RUN={dry_run}, lazer={lazer_on} (firing gated; see banner)");

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
        // Detect→submit ~0 when armed (blockhash-stamp + send, no build/quote/sim).
        // Dormant until `try_arm` can populate the cache (fire math unsolved).
        for v in &cands {
            if let Some(a) = arm_cache.get(&v.config.vault_id) {
                let submit_us = now_us();
                log_latency(&run_dir, &serde_json::json!({
                    "event": "fire", "protocol": "jupiter", "vault_id": v.config.vault_id,
                    "quoted_out": a.quoted_out, "armed_age_us": (submit_us - a.built_us).to_string(),
                    "submit_us": submit_us.to_string(), "dry_run": dry_run, "tx_bytes": a.tx.len(),
                }));
                // (send path wires here once arming is live — identical to the
                // marginfi executor's submit-only branch.)
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
            match resolve_liquidate_accounts(&endpoint, &v.config_pubkey) {
                Some((accts, src_n, captured_col)) => match accounts_from_captured(v, &accts) {
                    Some(la) => {
                        println!("     ✓ resolved liquidate accounts from a real tx (+{} remaining, {src_n} oracle sources)", la.remaining.len());
                        // Off-band ARM: build the priced fire tx (col_per_unit_debt +
                        // fresh remaining accounts) and sim-gate it.
                        match try_arm(&endpoint, v, &accts, src_n, captured_col) {
                            Some(armed) => { println!("     ✓ ARMED — priced fire tx simulates clean ({}B)", armed.tx.len());
                                arm_cache.insert(c.vault_id, armed); }
                            None => println!("     · not armed: not fireable at the live price (or non-USDC debt / \
                                captured col=0 so tick band not reconstructable) — sim-gated, not sending"),
                        }
                    }
                    None => println!("     ⚠ captured tx had <26 accounts; cannot map"),
                },
                None => println!("     · no recent liquidate tx references this vault_config \
                    — Liquidity PDAs not liftable yet"),
            }
        }
    }
}
