//! Root-cause the health divergence: for one account marginfi calls healthy but
//! our maintenance_health calls underwater, print the per-bank breakdown (shares
//! · share_value · price · our maint weight → contribution) and scan each bank's
//! bytes for ALL "weight-like" i80f48 values (0<v<2) with offsets — revealing
//! whether an emode boosted-weight config is present beyond the 4 config weights.
//!
//! Usage: HELIUS_RPC=<url> ACCOUNT=<pubkey> cargo run --release --bin mfi_health_debug

use arb_engine::liquidation::{self as liq, Bank, MarginfiAccount};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::str::FromStr;
use std::time::Duration;

const DEFAULT_ACCOUNT: &str = "BH736MqzFt2dNMeytao6wDn9M1JtMYT2PJnrFxGzknUr";

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
fn get_multiple(endpoint: &str, keys: &[Pubkey]) -> HashMap<Pubkey, Vec<u8>> {
    let mut out = HashMap::new();
    let strs: Vec<String> = keys.iter().map(|k| k.to_string()).collect();
    if let Some(v) = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getMultipleAccounts",
        "params":[strs, {"encoding":"base64"}]})) {
        for (i, acc) in v["result"]["value"].as_array().into_iter().flatten().enumerate() {
            if let Some(b) = acc.get("data").and_then(b64) { out.insert(keys[i], b); }
        }
    }
    out
}
fn get_one(endpoint: &str, pk: &Pubkey) -> Option<Vec<u8>> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
        "params":[pk.to_string(), {"encoding":"base64"}]}))?;
    b64(&v["result"]["value"]["data"])
}

fn i80f48(bytes: &[u8], off: usize) -> Option<f64> {
    let s = bytes.get(off..off + 16)?;
    let mut buf = [0u8; 16]; buf.copy_from_slice(s);
    Some(i128::from_le_bytes(buf) as f64 / (1u64 << 48) as f64)
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let account = Pubkey::from_str(&std::env::var("ACCOUNT").unwrap_or_else(|_| DEFAULT_ACCOUNT.into())).unwrap();

    let raw = get_one(&endpoint, &account).expect("account");
    let a = MarginfiAccount::decode(&raw).expect("decode account");
    println!("account {account}\n  authority {}\n  {} active balances", a.authority, a.balances.len());

    // Account bytes after balances (1736..) — flags + emode region.
    println!("  ── account tail (post-balances @1736, len {}) ──", raw.len());
    if raw.len() >= 1736 {
        let tail = &raw[1736..];
        // account_flags is a u64 right after balances in marginfi v2.
        if let Ok(f) = tail.get(0..8).map(|b| u64::from_le_bytes(b.try_into().unwrap())).ok_or(()) {
            println!("     account_flags @1736 = {f} (0x{f:x})");
        }
        // Scan the tail for u16 values that could be an emode_tag (small nonzero).
        let tags: Vec<(usize, u16)> = (0..tail.len().saturating_sub(2)).step_by(2)
            .map(|i| (1736 + i, u16::from_le_bytes(tail[i..i+2].try_into().unwrap())))
            .filter(|(_, v)| *v > 0 && *v < 4096).take(8).collect();
        println!("     small-u16 (emode_tag candidates) in tail: {:?}", tags);
    }

    let bank_pks: Vec<Pubkey> = a.balances.iter().map(|b| b.bank_pk).collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks = HashMap::new();
    for (pk, r) in &bank_raw { if let Some(bk) = Bank::decode(r) { banks.insert(*pk, bk); } }

    // Prices from each bank's oracle.
    let oracle_pks: Vec<Pubkey> = banks.values().map(|b| b.oracle_key).collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut price_of: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, r) in &oracle_raw { if let Some(p) = liq::decode_oracle_price(r) { price_of.insert(*pk, p); } }

    println!("\n  ── per-balance health breakdown (our maintenance_health) ──");
    let (mut wa, mut wl) = (0.0f64, 0.0f64);
    for b in &a.balances {
        let Some(bank) = banks.get(&b.bank_pk) else { println!("    {} … BANK MISSING", &b.bank_pk.to_string()[..8]); continue };
        let price = price_of.get(&bank.oracle_key).copied().unwrap_or(f64::NAN);
        let scale = 10f64.powi(bank.mint_decimals as i32);
        if b.asset_shares > 0.0 {
            let ui = b.asset_shares * bank.asset_share_value / scale;
            let contrib = ui * price * bank.asset_weight_maint;
            wa += contrib;
            println!("    ASSET {}…  ui={:.4} price=${:.4} w_maint={:.4} (w_init={:.4}) → ${:.2}",
                &b.bank_pk.to_string()[..8], ui, price, bank.asset_weight_maint, bank.asset_weight_init, contrib);
        }
        if b.liability_shares > 0.0 {
            let ui = b.liability_shares * bank.liability_share_value / scale;
            let contrib = ui * price * bank.liability_weight_maint;
            wl += contrib;
            println!("    LIAB  {}…  ui={:.4} price=${:.4} w_maint={:.4} → ${:.2}",
                &b.bank_pk.to_string()[..8], ui, price, bank.liability_weight_maint, contrib);
        }
    }
    println!("  → [no-emode] weighted_assets ${:.2}  weighted_liabilities ${:.2}  ratio {:.4}  {}",
        wa, wl, if wa > 0.0 { wl / wa } else { f64::INFINITY },
        if wa < wl { "UNDERWATER" } else { "healthy" });

    // Emode-aware verdict via the production maintenance_health (should match marginfi).
    let price_map: HashMap<Pubkey, f64> = banks.iter()
        .filter_map(|(pk, bk)| Some((*pk, *price_of.get(&bk.oracle_key)?))).collect();
    let r = liq::maintenance_health(&a, &banks, &price_map);
    println!("  → [emode]    weighted_assets ${:.2}  weighted_liabilities ${:.2}  ratio {:.4}  {} (missing {})",
        r.health.weighted_assets, r.health.weighted_liabilities, r.health.ratio(),
        if r.health.liquidatable() { "UNDERWATER" } else { "healthy" }, r.missing);
    // What asset-weight boost on the collateral would make marginfi's verdict (healthy) consistent?
    if wa > 0.0 && wa < wl {
        println!("  → to be healthy, collateral asset-weight would need ≈{:.2}× boost (emode?)", wl / wa);
    }

    // Emode decode at the hypothesized layout: EmodeSettings starts @1240
    // (emode_tag u16), emode entries[10] start @1264, each 40 bytes:
    // collateral_bank_emode_tag u16 @0, asset_weight_init @8, asset_weight_maint @24.
    const EMODE_ENTRIES: usize = 1264;
    const ENTRY_SIZE: usize = 40;
    for (pk, rawbank) in &bank_raw {
        let role = if a.balances.iter().any(|b| b.bank_pk == *pk && b.liability_shares > 0.0) { "LIAB" } else { "ASSET" };
        println!("\n  ── {role} bank {} ──", &pk.to_string()[..8]);
        // Hunt for this bank's own emode_tag: print every u16 in 880..1268 so we
        // can see where 619/871 (the tags USDC references) sit for the collateral.
        let tagline: Vec<(usize, u16)> = (880..1268).step_by(2)
            .filter_map(|i| rawbank.get(i..i+2).map(|b| (i, u16::from_le_bytes(b.try_into().unwrap()))))
            .filter(|(_, v)| *v > 0 && *v < 60000).collect();
        println!("     u16 in 880..1268 (nonzero): {tagline:?}");
        // Entries (only the clean, in-range ones).
        for e in 0..10 {
            let base = EMODE_ENTRIES + e * ENTRY_SIZE;
            let Some(tag) = rawbank.get(base..base+2).map(|b| u16::from_le_bytes(b.try_into().unwrap())) else { break };
            let init = i80f48(rawbank, base + 8).unwrap_or(0.0);
            let maint = i80f48(rawbank, base + 24).unwrap_or(0.0);
            if (0.0..=2.0).contains(&init) && (0.0..=2.0).contains(&maint) && (init > 0.0 || maint > 0.0) {
                println!("     entry[{e}] collat_tag={tag}  w_init={init:.4}  w_maint={maint:.4}");
            }
        }
    }
}
