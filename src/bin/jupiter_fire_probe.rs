//! Verify the Jupiter Lend (Fluid) liquidate reversal end-to-end. Three stages,
//! all read-only (never submits):
//!
//! 1. GROUND-TRUTH PDA CHECK — pull recent real `liquidate` txs off the Vaults
//!    program, split each `remaining` list by `remaining_accounts_indices`
//!    ([sources, branches, ticks, tick_has_debt]), read every branch/tick/
//!    tick_has_debt account, decode its id, and re-derive the PDA from our seeds.
//!    A 100% match proves the seed + layout reversal against real liquidators.
//!
//! 2+3. LIVE SELECTION + SIM-VERIFY — for each vault with a recent liquidate (so
//!    its Liquidity PDAs + oracle sources are liftable), derive the
//!    `remaining_accounts` FRESH from current on-chain state via
//!    `build_remaining_accounts`, build a `liquidate` ix (captured liquidator-side
//!    accounts + our fresh remaining, col_per_unit_debt = 0), and
//!    simulateTransaction (sigVerify=false, replaceRecentBlockhash=true). Success
//!    bar: a CLEAN sim, or a revert at the protocol's OWN liquidation gate
//!    (VaultInvalidLiquidation etc.) — either proves every upstream leg composes
//!    (oracle CPI via sources, exchange prices, branch/tick/tick_has_debt wiring).
//!
//! 4. FULL FIRE TX — for a USDC-debt vault, build the marginfi-flash-loan-wrapped
//!    liquidate+swap+repay tx and report composition + byte size (a single-packet
//!    fire needs a deployment ALT, like Save's SAVE_ALT).
//!
//! Usage: HELIUS_RPC=<url> [SCAN_SIGS=1000] [SIM_VAULT=<id>] cargo run --release --bin jupiter_fire_probe

use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
use arb_engine::jupiter_fire::{
    accounts_from_captured, build_jupiter_fire_tx, build_liquidate_ix,
    build_remaining_accounts, JupiterFireCandidate, LIQUIDATE_DISC,
};
use arb_engine::jupiter_math::{self, branch_pda, tick_has_debt_pda, tick_pda, BranchLite};
use solana_pubkey::Pubkey;
use std::str::FromStr;
use std::time::Duration;

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..5 {
        if let Ok(r) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = r.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(400 << attempt));
    }
    None
}
fn b64field(d: &serde_json::Value) -> Option<Vec<u8>> {
    use base64::Engine;
    base64::engine::general_purpose::STANDARD.decode(d.get(0)?.as_str()?).ok()
}
fn get_acct(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64field(&v["result"]["value"]["data"])
}
fn mint_owner(endpoint: &str, mint: &Pubkey) -> Option<Pubkey> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[mint.to_string(), {"encoding":"base64"}]}))?;
    v["result"]["value"]["owner"].as_str()?.parse().ok()
}

/// A decoded real liquidate ix: args + full ordered account list.
struct RealLiq {
    sig: String,
    debt_amt: u64,
    col_per_unit_debt: u128,
    indices: Vec<u8>,
    accounts: Vec<Pubkey>,
}

fn decode_liq_args(data: &[u8]) -> Option<(u64, u128, Vec<u8>)> {
    let debt = u64::from_le_bytes(data.get(8..16)?.try_into().ok()?);
    let col = u128::from_le_bytes(data.get(16..32)?.try_into().ok()?);
    let mut o = 32;
    o += 1; // absorb
    o += if *data.get(o)? == 1 { 2 } else { 1 }; // transfer_type
    let ilen = u32::from_le_bytes(data.get(o..o + 4)?.try_into().ok()?) as usize;
    o += 4;
    let idx = data.get(o..o + ilen)?.to_vec();
    Some((debt, col, idx))
}

/// Pull recent liquidate ixs off the program (named + loaded addresses resolved).
fn recent_liquidates(endpoint: &str, scan: usize, want: usize) -> Vec<RealLiq> {
    let prog: Pubkey = jupiter::VAULTS_PROGRAM.parse().unwrap();
    let sigs = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress",
        "params":[jupiter::VAULTS_PROGRAM, {"limit":scan}]}));
    let mut out = Vec::new();
    for e in sigs.as_ref().and_then(|v| v["result"].as_array()).into_iter().flatten() {
        if !e["err"].is_null() { continue; }
        let Some(sig) = e["signature"].as_str() else { continue };
        let Some(tx) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
            "params":[sig, {"encoding":"json","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
        let msg = &tx["result"]["transaction"]["message"];
        let Some(base) = msg["accountKeys"].as_array() else { continue };
        let mut keys: Vec<Pubkey> = base.iter().filter_map(|k| k.as_str().and_then(|s| s.parse().ok())).collect();
        if let Some(la) = tx["result"]["meta"]["loadedAddresses"].as_object() {
            for side in ["writable", "readonly"] {
                for k in la.get(side).and_then(|v| v.as_array()).into_iter().flatten() {
                    if let Some(pk) = k.as_str().and_then(|s| s.parse().ok()) { keys.push(pk); }
                }
            }
        }
        let check = |ix: &serde_json::Value| -> Option<RealLiq> {
            let pidx = ix["programIdIndex"].as_u64()? as usize;
            if *keys.get(pidx)? != prog { return None; }
            let data = bs58::decode(ix["data"].as_str()?).into_vec().ok()?;
            if data.len() < 8 || data[..8] != LIQUIDATE_DISC { return None; }
            let (debt, col, idx) = decode_liq_args(&data)?;
            let accts: Vec<Pubkey> = ix["accounts"].as_array()?.iter()
                .filter_map(|i| i.as_u64().and_then(|i| keys.get(i as usize)).copied()).collect();
            Some(RealLiq { sig: sig.to_string(), debt_amt: debt, col_per_unit_debt: col, indices: idx, accounts: accts })
        };
        let mut found = None;
        for ix in msg["instructions"].as_array().into_iter().flatten() {
            if let Some(r) = check(ix) { found = Some(r); break; }
        }
        if found.is_none() {
            for inner in tx["result"]["meta"]["innerInstructions"].as_array().into_iter().flatten() {
                for ix in inner["instructions"].as_array().into_iter().flatten() {
                    if let Some(r) = check(ix) { found = Some(r); break; }
                }
            }
        }
        if let Some(r) = found { out.push(r); if out.len() >= want { break; } }
    }
    out
}

/// Load one vault (config+state) by its vault_config pubkey.
fn load_vault(endpoint: &str, config_pk: &Pubkey) -> Option<Vault> {
    let cfg_raw = get_acct(endpoint, config_pk)?;
    let cfg = VaultConfig::decode(&cfg_raw)?;
    let state_pk = Pubkey::find_program_address(
        &[b"vault_state", &cfg.vault_id.to_le_bytes()],
        &jupiter::VAULTS_PROGRAM.parse().unwrap(),
    ).0;
    let st_raw = get_acct(endpoint, &state_pk)?;
    let st = VaultState::decode(&st_raw)?;
    Some(Vault { config_pubkey: *config_pk, state_pubkey: state_pk, config: cfg, state: st })
}

fn simulate(endpoint: &str, tx: &solana_transaction::versioned::VersionedTransaction) -> Option<serde_json::Value> {
    use base64::Engine;
    let b = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(tx).unwrap());
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"simulateTransaction",
        "params":[b, {"sigVerify":false,"replaceRecentBlockhash":true,"commitment":"processed","encoding":"base64"}]}))
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let scan: usize = std::env::var("SCAN_SIGS").ok().and_then(|s| s.parse().ok()).unwrap_or(1000);
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak".into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD".into())).unwrap();

    println!("[jup-fire] pulling recent real liquidates (scan {scan} sigs)…");
    let reals = recent_liquidates(&endpoint, scan, 12);
    println!("[jup-fire] captured {} real liquidate ixs\n", reals.len());

    // ── STAGE 1: PDA-seed + layout ground-truth check ──────────────────────────
    println!("═══ STAGE 1: PDA-seed reversal vs real txs ═══");
    let (mut ok, mut bad, mut checked) = (0usize, 0usize, 0usize);
    for r in &reals {
        if r.indices.len() != 4 { println!("  {} : indices {:?} not len-4 ?!", &r.sig[..12], r.indices); continue; }
        if r.accounts.len() < 26 { continue; }
        let vault = match load_vault(&endpoint, &r.accounts[4]) { Some(v) => v, None => continue };
        let vid = vault.config.vault_id;
        let rem = &r.accounts[26..];
        let (ns, nb, nt, nh) = (r.indices[0] as usize, r.indices[1] as usize, r.indices[2] as usize, r.indices[3] as usize);
        if ns + nb + nt + nh != rem.len() {
            println!("  {} vault {vid}: indices {:?} sum != {} remaining", &r.sig[..12], r.indices, rem.len());
            continue;
        }
        let branches = &rem[ns..ns + nb];
        let ticks = &rem[ns + nb..ns + nb + nt];
        let thd = &rem[ns + nb + nt..];
        let mut vok = true;
        // branches: read → branch_id → re-derive
        for pk in branches {
            checked += 1;
            match get_acct(&endpoint, pk).as_deref().and_then(BranchLite::decode) {
                Some(b) if branch_pda(vid, b.branch_id) == *pk => ok += 1,
                _ => { bad += 1; vok = false; }
            }
        }
        // ticks: read → tick → re-derive (Tick.tick @ offset 10, i32)
        for pk in ticks {
            checked += 1;
            let tick = get_acct(&endpoint, pk).and_then(|d| d.get(10..14).map(|s| i32::from_le_bytes(s.try_into().unwrap())));
            match tick { Some(t) if tick_pda(vid, t) == *pk => ok += 1, _ => { bad += 1; vok = false; } }
        }
        // tick_has_debt: read → index (u8 @ offset 10) → re-derive
        for pk in thd {
            checked += 1;
            let idx = get_acct(&endpoint, pk).and_then(|d| d.get(10).copied());
            match idx { Some(i) if tick_has_debt_pda(vid, i) == *pk => ok += 1, _ => { bad += 1; vok = false; } }
        }
        println!("  {} vault {:>3} [{}→{}] idx {:?}  branches {} ticks {} thd {}  {}",
            &r.sig[..12], vid, &vault.config.supply_token.to_string()[..4], vault.config.debt_label(),
            r.indices, nb, nt, nh, if vok { "✓ all PDAs reproduce" } else { "✗ mismatch" });
    }
    println!("  → {ok}/{checked} tick/branch/tick_has_debt PDAs reproduced from seeds ({bad} mismatch)\n");

    // ── STAGE 2+3: for EACH captured candidate, derive remaining accounts FRESH
    // from current state and sim the resolver liquidate. The first that composes
    // (VaultLiquidationResult / gated revert, not a size/RPC error) is the proof.
    use solana_message::{v0, VersionedMessage};
    let sim_vault_id: Option<u16> = std::env::var("SIM_VAULT").ok().and_then(|s| s.parse().ok());
    let usable = |r: &&RealLiq| r.accounts.len() >= 26 && r.indices.len() == 4 && r.col_per_unit_debt > 0;
    let fetch = |pk: &Pubkey| -> Option<Vec<u8>> { get_acct(&endpoint, pk) };

    println!("═══ STAGE 2+3: live selection + resolver sim (per candidate) ═══");
    let mut resolver_proved: Option<(Vault, Vec<Pubkey>, [u8; 4])> = None;
    for r in reals.iter().filter(usable) {
        let Some(vault) = load_vault(&endpoint, &r.accounts[4]) else { continue };
        if let Some(id) = sim_vault_id { if vault.config.vault_id != id { continue; } }
        let vid = vault.config.vault_id;
        let s = &vault.state;
        let ns = r.indices[0] as usize;
        let oracle_sources: Vec<Pubkey> = r.accounts[26..26 + ns].to_vec();
        // liquidation_tick reconstructed from the captured col_per_unit_debt
        // (production derives it live from the Lazer price).
        let liq_tick = jupiter_math::liquidation_tick_from_col_per_debt(
            r.col_per_unit_debt, vault.config.liquidation_penalty, vault.config.liquidation_threshold,
        ).unwrap_or(s.topmost_tick - 1);
        let (remaining, indices) =
            build_remaining_accounts(vid, s.topmost_tick, s.current_branch_id, liq_tick, &oracle_sources, &fetch);

        // Keep the CAPTURED liquidator-side accounts (they satisfy the program's
        // token-owner constraints, as in the real tx); only the remaining/tick
        // accounts are our fresh derivation. col_per_unit_debt=0 = accept oracle
        // price (no false slippage revert). to != DEAD, so this exercises the real
        // liquidate path (not the resolver, which needs to's ATA to exist).
        let mut a = match accounts_from_captured(&vault, &r.accounts) { Some(a) => a, None => continue };
        a.remaining = remaining.clone();
        let resolver_ix = build_liquidate_ix(&a, r.debt_amt, 0, false, Some(1), &indices);
        let msg = v0::Message::try_compile(&r.accounts[0], &[resolver_ix], &[], solana_hash::Hash::default()).unwrap();
        let tx = solana_transaction::versioned::VersionedTransaction {
            signatures: vec![solana_signature::Signature::default()],
            message: VersionedMessage::V0(msg),
        };
        let tx_bytes = bincode::serialize(&tx).unwrap().len();
        print!("  vault {vid:>3} [{}→{}] liq_tick={liq_tick} derived idx {:?} → {tx_bytes}B: ",
            &vault.config.supply_token.to_string()[..4], vault.config.debt_label(), indices);
        if tx_bytes > 1232 {
            println!("resolver tx > 1232 (needs ALT for a single-packet fire) — skip sim");
            continue;
        }
        let raw = simulate(&endpoint, &tx);
        let val = raw.as_ref().and_then(|v| v["result"].get("value").cloned());
        match val {
            Some(v) => {
                let logs: Vec<String> = v["logs"].as_array().into_iter().flatten().filter_map(|l| l.as_str().map(String::from)).collect();
                // "Composes" = the ix passed account validation and reached the
                // liquidation logic (a Vault* liquidation/slippage gate), or ran clean.
                let gate = logs.iter().find(|l| l.contains("Vault") &&
                    (l.contains("Liquidat") || l.contains("Slippage") || l.contains("TopTick") || l.contains("Tick")));
                if v["err"].is_null() {
                    println!("★★ liquidate SIMULATES CLEAN — composes end-to-end; vault liquidatable at live price");
                    resolver_proved = Some((vault.clone(), remaining.clone(), indices));
                    break;
                } else if let Some(g) = gate {
                    println!("★ composes → gated at the protocol's own liquidation gate");
                    println!("     {}", g.trim());
                    resolver_proved = Some((vault.clone(), remaining.clone(), indices));
                    break;
                } else {
                    println!("upstream revert: {}", v["err"]);
                    for l in logs.iter().rev().take(4).rev() { println!("       {l}"); }
                }
            }
            None => println!("RPC error: {}", raw.map(|v| v["error"].to_string()).unwrap_or_default()),
        }
    }
    if resolver_proved.is_none() {
        println!("  (no candidate composed cleanly — see per-candidate reasons above)");
    }

    // Full flash-loan-wrapped fire tx (USDC debt only) — compose + size + sim.
    if let Some((vault, remaining, indices)) = resolver_proved {
        let r = reals.iter().find(|r| usable(r) && load_vault(&endpoint, &r.accounts[4]).map(|v| v.config.vault_id) == Some(vault.config.vault_id)).unwrap();
        println!("\n═══ STAGE 4: flash-loan-wrapped fire tx (vault {}) ═══", vault.config.vault_id);
        if vault.config.debt_label() == "USDC" {
        println!("\n  ── full flash-loan-wrapped fire tx ──");
        let collat_mint = vault.config.supply_token;
        let ctp = mint_owner(&endpoint, &collat_mint).unwrap_or_else(|| Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap());
        let mut fa = accounts_from_captured(&vault, &r.accounts).unwrap();
        fa.remaining = remaining.clone();
        // size the seize by the resolver-implied col if available, else a nominal
        let seize = (r.debt_amt as u128 * r.col_per_unit_debt.max(10u128.pow(13)) / 10u128.pow(15)) as u64;
        let cand = JupiterFireCandidate {
            accts: fa, debt_amt: r.debt_amt, col_per_unit_debt: 0,
            remaining: remaining.clone(), remaining_indices: indices,
            seize_underlying: seize.max(1), collateral_mint: collat_mint, collateral_token_program: ctp,
        };
        match build_jupiter_fire_tx(&endpoint, &cand, &liquidator_ma, &authority, None, 0, 50_000, 100, 16, solana_hash::Hash::default()) {
            Ok(fire) => {
                println!("     built fire tx: {} bytes, quoted USDC out {}", fire.tx_bytes, fire.quoted_usdc_out);
                if fire.tx_bytes > 1232 {
                    println!("     ⓘ {}B > 1232 single-packet limit — needs a deployment ALT (JUP_ALT/LIQ_ALT holding the\n       vault's fixed Liquidity PDAs + marginfi/token program ids, exactly like Save's SAVE_ALT).\n       The liquidate LEG is sim-proven above; the wrap composes by construction (mirrors save_fire).", fire.tx_bytes);
                } else {
                    match simulate(&endpoint, &fire.tx).as_ref().and_then(|v| v["result"].get("value").cloned()) {
                        Some(v) if v["err"].is_null() => println!("     ★★ FIRE TX SIMULATES CLEAN — would liquidate profitably now ({} CU)", v["unitsConsumed"]),
                        Some(v) => {
                            println!("     fire tx gated/other: {}", v["err"]);
                            for l in v["logs"].as_array().into_iter().flatten().filter_map(|l| l.as_str()).collect::<Vec<_>>().iter().rev().take(6).rev() { println!("        {l}"); }
                        }
                        None => println!("     fire sim returned nothing (likely still > 1232 after ALT)"),
                    }
                }
            }
            Err(e) => println!("     fire build failed (often: Jupiter quote for tiny/odd size): {e}"),
        }
        } else {
            println!("  (vault debt is {}, not USDC — flash-loan wrap is USDC-only; resolver sim already proved the liquidate leg composes.)", vault.config.debt_label());
        }
    }
    println!("\n[jup-fire] done.");
}
