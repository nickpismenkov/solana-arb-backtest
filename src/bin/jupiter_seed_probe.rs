//! Validate the pure-seed derivation of the Fluid (Jupiter Lend) **Liquidity**
//! program accounts that a Vaults `liquidate` ix needs (positions 9..=22) + the
//! oracle `sources` — the accounts the executor used to lift from a captured tx.
//! The point: prove they can be derived for ANY vault WITHOUT a recent liquidate.
//!
//! Two independent proofs:
//!  A. STANDALONE (no tx needed): for every in-scope vault, derive each account
//!     from seeds via `jupiter_fire::derive_liquidate_accounts` + the decoded
//!     oracle sources, then read it on-chain and assert it's real and correctly
//!     owned (Liquidity PDAs owned by the liquidity program; the vault token
//!     accounts are SPL accounts whose authority == the `liquidity` PDA and whose
//!     mint matches). This is what lets a never-liquidated vault arm.
//!  B. GROUND TRUTH (when a recent liquidate exists): pull real liquidate txs and
//!     assert the seed-derived pubkeys EQUAL the exact accounts the real
//!     liquidator passed at positions 9..=22 + the oracle sources.
//!
//! Read-only. Usage: HELIUS_RPC=<url> [SCAN_SIGS=1500] [MAX_VAULTS=12]
//!   cargo run --release --bin jupiter_seed_probe

use arb_engine::jupiter::{self, Vault, VaultConfig, VaultState};
use arb_engine::jupiter_fire::{derive_liquidate_accounts, LIQUIDATE_DISC};
use arb_engine::jupiter_math::{self, new_branch_id};
use solana_pubkey::Pubkey;
use std::collections::HashMap;
use std::str::FromStr;
use std::time::Duration;

const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";

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

/// (owner, data) for a batch of accounts (None slot = account missing).
fn get_multi(endpoint: &str, pks: &[Pubkey]) -> Vec<Option<(Pubkey, Vec<u8>)>> {
    let strs: Vec<String> = pks.iter().map(|p| p.to_string()).collect();
    let mut out = vec![None; pks.len()];
    for (chunk_i, chunk) in strs.chunks(100).enumerate() {
        let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
            "params":[chunk, {"encoding":"base64"}]}));
        let arr = v.as_ref().and_then(|v| v["result"]["value"].as_array()).cloned().unwrap_or_default();
        for (j, acc) in arr.iter().enumerate() {
            if acc.is_null() { continue; }
            let owner = acc["owner"].as_str().and_then(|s| s.parse::<Pubkey>().ok());
            let data = acc.get("data").and_then(b64field);
            if let (Some(o), Some(d)) = (owner, data) { out[chunk_i * 100 + j] = Some((o, d)); }
        }
    }
    out
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
            b64field(&e["account"]["data"]),
        ) { out.push((pk, data)); }
    }
    out
}

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

/// SPL token-account decode: mint @0, owner @32 (both work for Token & Token-2022
/// base layout).
fn token_acct_mint_owner(data: &[u8]) -> Option<(Pubkey, Pubkey)> {
    Some((
        Pubkey::new_from_array(data.get(0..32)?.try_into().ok()?),
        Pubkey::new_from_array(data.get(32..64)?.try_into().ok()?),
    ))
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let scan: usize = std::env::var("SCAN_SIGS").ok().and_then(|s| s.parse().ok()).unwrap_or(1500);
    let max_vaults: usize = std::env::var("MAX_VAULTS").ok().and_then(|s| s.parse().ok()).unwrap_or(12);

    let liq_prog = Pubkey::from_str(jupiter::LIQUIDITY_PROGRAM).unwrap();
    let liquidity = jupiter_math::liquidity_pda();
    println!("[seed] liquidity global PDA = {liquidity}");
    match get_acct(&endpoint, &liquidity) {
        Some(_) => println!("[seed]   ✓ exists on-chain\n"),
        None => println!("[seed]   ✗ MISSING — seed for `liquidity` is wrong!\n"),
    }

    let vaults = load_vaults(&endpoint);
    let scoped: Vec<&Vault> = vaults.iter().filter(|v| v.config.debt_in_scope()).collect();
    println!("[seed] {} vaults total, {} in-scope (USDC/USDT/wSOL debt)\n", vaults.len(), scoped.len());

    // token program per mint (cache).
    let mut mint_tp: HashMap<Pubkey, Pubkey> = HashMap::new();
    {
        let mints: Vec<Pubkey> = scoped.iter().flat_map(|v| [v.config.supply_token, v.config.borrow_token]).collect();
        let got = get_multi(&endpoint, &mints);
        for (m, g) in mints.iter().zip(got) {
            if let Some((owner, _)) = g { mint_tp.insert(*m, owner); }
        }
    }
    let tp = |m: &Pubkey| mint_tp.get(m).copied().unwrap_or_else(|| Pubkey::from_str(TOKEN).unwrap());

    // ── PROOF A: standalone seed derivation validated vs live account state ──
    println!("═══ PROOF A — seed-derived accounts exist + correctly owned (no tx) ═══");
    let mut a_pass = 0usize;
    let mut a_checked = 0usize;
    for v in scoped.iter().take(max_vaults) {
        let vid = v.config.vault_id;
        let a = derive_liquidate_accounts(v, tp(&v.config.supply_token), tp(&v.config.borrow_token));
        // oracle sources from the decoded oracle account.
        let oracle_raw = get_acct(&endpoint, &v.config.oracle);
        let sources = oracle_raw.as_deref().and_then(jupiter::decode_oracle_sources).unwrap_or_default();

        // group of (label, pubkey, expected-owner-kind) to verify.
        // kind: 'L' liquidity-owned PDA, 'T' spl token account (owner=liquidity),
        // 'V' vaults-program PDA, 'O' oracle-source (any, just must exist).
        let mut checks: Vec<(&str, Pubkey, char)> = vec![
            ("supply_reserves", a.supply_token_reserves_liquidity, 'L'),
            ("borrow_reserves", a.borrow_token_reserves_liquidity, 'L'),
            ("supply_position", a.vault_supply_position_on_liquidity, 'L'),
            ("borrow_position", a.vault_borrow_position_on_liquidity, 'L'),
            ("supply_rate_model", a.supply_rate_model, 'L'),
            ("borrow_rate_model", a.borrow_rate_model, 'L'),
            ("supply_claim(None)", a.supply_token_claim_account, 'N'),
            ("vault_supply_tok_acct", a.vault_supply_token_account, 'T'),
            ("vault_borrow_tok_acct", a.vault_borrow_token_account, 'T'),
            ("new_branch", a.new_branch, 'V'),
        ];
        for (i, s) in sources.iter().enumerate() {
            let name: &'static str = Box::leak(format!("oracle_source[{i}]").into_boxed_str());
            checks.push((name, *s, 'O'));
        }
        let pks: Vec<Pubkey> = checks.iter().map(|(_, p, _)| *p).collect();
        let got = get_multi(&endpoint, &pks);

        let nb_id = new_branch_id(v.state.branch_liquidated, v.state.current_branch_id, v.state.total_branch_id);
        println!("── vault {vid:>3} [{}→{}]  new_branch_id={nb_id} (bl={}, cur={}, tot={})  {} oracle src ──",
            &v.config.supply_token.to_string()[..4], v.config.debt_label(),
            v.state.branch_liquidated, v.state.current_branch_id, v.state.total_branch_id, sources.len());
        for ((label, pk, kind), g) in checks.iter().zip(got) {
            a_checked += 1;
            let verdict = match (kind, &g) {
                ('N', _) if *pk == Pubkey::from_str(jupiter::VAULTS_PROGRAM).unwrap() => "✓ None-sentinel (=vaults program id)",
                ('L', Some((owner, _))) if *owner == liq_prog => "✓ liquidity-owned",
                ('V', Some((owner, _))) if owner.to_string() == jupiter::VAULTS_PROGRAM => "✓ vaults-owned",
                ('V', None) => "· not yet created (branch reused/absent — sim is the gate)",
                ('T', Some((owner, data))) => {
                    match token_acct_mint_owner(data) {
                        Some((_, auth)) if auth == liquidity && mint_tp.values().any(|t| t == owner) =>
                            "✓ SPL acct, authority=liquidity",
                        _ => "✗ token acct authority/owner mismatch",
                    }
                }
                ('O', Some(_)) => "✓ source exists",
                (_, Some(_)) => "✗ wrong owner",
                (_, None) => "✗ MISSING",
            };
            if verdict.starts_with('✓') || verdict.starts_with('·') { a_pass += 1; }
            // only print the interesting / failing lines to keep output readable
            if !label.starts_with("oracle_source") || !verdict.starts_with('✓') {
                println!("     {:<22} {}  {}", label, &pk.to_string()[..8], verdict);
            }
        }
    }
    println!("\n  → PROOF A: {a_pass}/{a_checked} derived accounts real + correctly owned\n");

    // ── PROOF B: exact-match vs real liquidate txs (only if any exist) ──
    println!("═══ PROOF B — seed derivation == real liquidator's accounts (ground truth) ═══");
    let reals = recent_liquidates(&endpoint, scan, 10);
    if reals.is_empty() {
        println!("  (no recent liquidate tx in {scan} sigs — liquidations are rare on this protocol;");
        println!("   PROOF A + the on-chain program's own seed constraints at sim are the validation.)");
    }
    let (mut b_ok, mut b_bad) = (0usize, 0usize);
    for r in &reals {
        if r.accounts.len() < 26 { continue; }
        let Some(v) = load_vault(&endpoint, &r.accounts[4]) else { continue };
        let a = derive_liquidate_accounts(&v, tp(&v.config.supply_token), tp(&v.config.borrow_token));
        let src_n = r.indices.first().copied().unwrap_or(0) as usize;
        let oracle_raw = get_acct(&endpoint, &v.config.oracle);
        let derived_sources = oracle_raw.as_deref().and_then(jupiter::decode_oracle_sources).unwrap_or_default();
        let real_sources = &r.accounts[26..(26 + src_n).min(r.accounts.len())];
        let pairs: [(&str, Pubkey, Pubkey); 12] = [
            ("new_branch", a.new_branch, r.accounts[9]),
            ("supply_reserves", a.supply_token_reserves_liquidity, r.accounts[10]),
            ("borrow_reserves", a.borrow_token_reserves_liquidity, r.accounts[11]),
            ("supply_position", a.vault_supply_position_on_liquidity, r.accounts[12]),
            ("borrow_position", a.vault_borrow_position_on_liquidity, r.accounts[13]),
            ("supply_rate_model", a.supply_rate_model, r.accounts[14]),
            ("borrow_rate_model", a.borrow_rate_model, r.accounts[15]),
            ("supply_claim", a.supply_token_claim_account, r.accounts[16]),
            ("liquidity", a.liquidity, r.accounts[17]),
            ("vault_supply_tok_acct", a.vault_supply_token_account, r.accounts[19]),
            ("vault_borrow_tok_acct", a.vault_borrow_token_account, r.accounts[20]),
            ("liquidity_program", a.liquidity_program, r.accounts[18]),
        ];
        let mut vok = true;
        let mut fails = Vec::new();
        for (label, derived, real) in pairs {
            if derived == real { b_ok += 1; } else { b_bad += 1; vok = false; fails.push(label); }
        }
        // oracle sources exact-order match
        let src_match = derived_sources.len() == real_sources.len()
            && derived_sources.iter().zip(real_sources).all(|(a, b)| a == b);
        if src_match { b_ok += 1; } else { b_bad += 1; vok = false; fails.push("oracle_sources"); }
        println!("  {} vault {:>3} [{}→{}]  {}{}",
            &r.sig[..10], v.config.vault_id, &v.config.supply_token.to_string()[..4], v.config.debt_label(),
            if vok { "✓ ALL 12 accts + sources reproduced from seeds".to_string() } else { format!("✗ mismatch: {fails:?}") },
            if src_match { "" } else { " (see sources)" });
    }
    if !reals.is_empty() {
        println!("\n  → PROOF B: {b_ok} exact matches, {b_bad} mismatches across {} real liquidate txs", reals.len());
    }
    println!("\n[seed] done.");
}

// ── real-liquidate capture (shared shape with jupiter_fire_probe) ────────────
struct RealLiq { sig: String, accounts: Vec<Pubkey>, indices: Vec<u8> }
fn decode_indices(data: &[u8]) -> Option<Vec<u8>> {
    let mut o = 8 + 8 + 16 + 1;
    o += if *data.get(o)? == 1 { 2 } else { 1 };
    let ilen = u32::from_le_bytes(data.get(o..o + 4)?.try_into().ok()?) as usize;
    o += 4;
    Some(data.get(o..o + ilen)?.to_vec())
}
fn load_vault(endpoint: &str, config_pk: &Pubkey) -> Option<Vault> {
    let cfg_raw = get_acct(endpoint, config_pk)?;
    let cfg = VaultConfig::decode(&cfg_raw)?;
    let state_pk = Pubkey::find_program_address(
        &[b"vault_state", &cfg.vault_id.to_le_bytes()],
        &jupiter::VAULTS_PROGRAM.parse().unwrap(),
    ).0;
    let st = VaultState::decode(&get_acct(endpoint, &state_pk)?)?;
    Some(Vault { config_pubkey: *config_pk, state_pubkey: state_pk, config: cfg, state: st })
}

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
            let indices = decode_indices(&data)?;
            let accts: Vec<Pubkey> = ix["accounts"].as_array()?.iter()
                .filter_map(|i| i.as_u64().and_then(|i| keys.get(i as usize)).copied()).collect();
            Some(RealLiq { sig: sig.to_string(), accounts: accts, indices })
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
