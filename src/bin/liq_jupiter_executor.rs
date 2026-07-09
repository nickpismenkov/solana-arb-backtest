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
//! come from `build_remaining_accounts`. DRY_RUN by default; never submits here.
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
//! Usage: HELIUS_RPC=<url> PYTH_LAZER_TOKEN=<tok> [RUN_DIR=.] [TICK_POLL_MS=1]
//!        [VAULT_REFRESH_SECS=30] [HEARTBEAT_SECS=10]
//!        cargo run --release --bin liq_jupiter_executor

use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
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

/// Read raw account bytes (used by the seed-derivation arm path: oracle decode,
/// mint-owner lookup, and `build_remaining_accounts`' PDA existence probes).
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
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
fn try_arm(endpoint: &str, v: &Vault) -> Option<Armed> {
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
    let authority: Pubkey = std::env::var("AUTHORITY").ok().and_then(|s| s.parse().ok())?;
    let liquidator_ma: Pubkey = std::env::var("LIQUIDATOR_MA").ok().and_then(|s| s.parse().ok())?;
    let fire = arb_engine::jupiter_fire::build_jupiter_fire_tx(
        endpoint, &cand, &liquidator_ma, &authority, None, 0, 50_000, 100, 16, solana_hash::Hash::default(),
    ).ok()?;
    // Submittable-size gate (HONEST): `simulateTransaction` does NOT enforce the
    // 1232-byte single-packet limit, but `sendTransaction` does. Never cache a tx
    // we couldn't actually submit — without JUP_ALT the wrapped fire is ~1.5-1.7KB.
    // Deploy JUP_ALT (see `jup_alt_print`); build_jupiter_fire_tx folds it in and
    // the tx drops under 1232. Skip-and-log here rather than arm an unsendable tx.
    if fire.tx_bytes > 1232 {
        eprintln!("     · vault {} priced+composes CLEAN but fire tx is {}B > 1232 — deploy JUP_ALT to arm",
            v.config.vault_id, fire.tx_bytes);
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
            // Off-band ARM: derive the FULL account set from seeds (no captured tx
            // needed), build the priced flash-loan fire tx, and sim-gate it.
            match try_arm(&endpoint, v) {
                Some(armed) => {
                    println!("     ✓ ARMED — seed-derived, priced fire tx simulates clean ({}B)", armed.tx.len());
                    arm_cache.insert(c.vault_id, armed);
                }
                None => println!("     · not armed: not fireable at the live price, non-USDC debt, \
                    or fire tx > 1232B (deploy JUP_ALT — see `cargo run --bin jup_alt_print`) — sim-gated, not sending"),
            }
        }
    }
}
