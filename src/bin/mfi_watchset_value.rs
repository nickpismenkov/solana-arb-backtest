//! ADDRESSABLE-MARKET census: how much money is actually within reach on marginfi?
//!
//! The liquidations that LAND on a calm day are dust (2026-07-14: 119 liquidations
//! across all 4 protocols moved $171 total). That says nothing about the size of
//! the opportunity when volatility hits — for that you have to look at the standing
//! borrower population, not the fills. This bins every marginfi borrower by distance
//! to liquidation and sums the collateral in each bin, so we can answer: "if the
//! market drops X%, how much collateral comes into liquidation range, and is any of
//! it big enough to be worth firing at?"
//!
//! Also reports how much of that collateral our fire path could actually TAKE (v1
//! shape: 1 collateral / 1 USDC|USDT|wSOL debt) vs. what it would skip.
//!
//! Uses the same decoders as the live executor (liquidation::maintenance_health,
//! on-chain oracle prices) so the numbers match what the bot sees.
//!
//! Usage: HELIUS_RPC=<url> [DROP_PCT=10] cargo run --release --bin mfi_watchset_value

use arb_engine::liquidation::{self as liq, Bank, BankMap, MarginfiAccount, PriceMap};
use solana_pubkey::Pubkey;
use std::collections::{HashMap, HashSet};
use std::time::Duration;

const MARGINFI_PROGRAM: &str = "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA";
const MARGINFI_GROUP: &str = "4qp6Fx6tnZkY5Wropq9wUYgtFxXKwE6viZxFHg3rdAG8";
const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const SOL_MINT: &str = "So11111111111111111111111111111111111111112";

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

/// Is this the shape the fire path can act on? 1 collateral / 1 stable-or-SOL debt.
fn is_fireable_shape(a: &MarginfiAccount, banks: &BankMap) -> bool {
    let assets: Vec<_> = a.balances.iter().filter(|b| b.asset_shares > 0.0).collect();
    let liabs: Vec<_> = a.balances.iter().filter(|b| b.liability_shares > 0.0).collect();
    if assets.len() != 1 || liabs.len() != 1 { return false; }
    let Some(lb) = banks.get(&liabs[0].bank_pk) else { return false };
    let m = lb.mint.to_string();
    m == USDC_MINT || m == USDT_MINT || m == SOL_MINT
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).expect("HELIUS_RPC");
    let drop_pct: f64 = std::env::var("DROP_PCT").ok().and_then(|s| s.parse().ok()).unwrap_or(10.0);

    eprintln!("scanning marginfi borrowers …");
    let resp = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getProgramAccounts",
        "params":[MARGINFI_PROGRAM, {"encoding":"base64","dataSlice":{"offset":0,"length":1736},
            "filters":[{"dataSize":liq::MA_SIZE},{"memcmp":{"offset":8,"bytes":MARGINFI_GROUP}}]}]})).expect("scan");
    let accts: Vec<(Pubkey, MarginfiAccount)> = resp["result"].as_array().cloned().unwrap_or_default().iter()
        .filter_map(|e| Some((e["pubkey"].as_str()?.parse().ok()?, MarginfiAccount::decode(&b64(&e["account"]["data"])?)?)))
        .filter(|(_, a): &(Pubkey, MarginfiAccount)| a.balances.iter().any(|b| b.liability_shares > 0.0))
        .collect();

    let bank_pks: Vec<Pubkey> = accts.iter().flat_map(|(_, a)| a.balances.iter().map(|b| b.bank_pk))
        .collect::<HashSet<_>>().into_iter().collect();
    let bank_raw = get_multiple(&endpoint, &bank_pks);
    let mut banks: BankMap = HashMap::new();
    let mut oracle_of = HashMap::new();
    for (pk, r) in &bank_raw {
        if let Some(bk) = Bank::decode(r) { oracle_of.insert(*pk, bk.oracle_key); banks.insert(*pk, bk); }
    }
    let oracle_pks: Vec<Pubkey> = oracle_of.values().copied().collect::<HashSet<_>>().into_iter().collect();
    let oracle_raw = get_multiple(&endpoint, &oracle_pks);
    let mut price_by_oracle: HashMap<Pubkey, f64> = HashMap::new();
    for (pk, r) in &oracle_raw {
        if let Some(p) = liq::decode_oracle_price(r) { price_by_oracle.insert(*pk, p); }
    }
    let prices: PriceMap = oracle_of.iter().filter_map(|(bk, oc)| Some((*bk, *price_by_oracle.get(oc)?))).collect();
    eprintln!("{} borrowers, {} banks priced\n", accts.len(), prices.len());

    // Health today, and health after an adverse move of DROP_PCT on every
    // non-stable collateral (the "what does a real selloff put in range" question).
    let stable = |m: &str| m == USDC_MINT || m == USDT_MINT;
    let mut shocked: PriceMap = prices.clone();
    for (bank_pk, bank) in &banks {
        if stable(&bank.mint.to_string()) { continue; }
        if let Some(p) = shocked.get_mut(bank_pk) { *p *= 1.0 - drop_pct / 100.0; }
    }

    struct Row { coll: f64, ratio: f64, ratio_shocked: f64, fireable: bool }
    let mut rows: Vec<Row> = Vec::new();
    for (_pk, a) in &accts {
        let now = liq::maintenance_health(a, &banks, &prices);
        if now.missing > 0 || now.health.weighted_assets <= 0.0 || now.health.weighted_liabilities <= 0.0 { continue; }
        let then = liq::maintenance_health(a, &banks, &shocked);
        // Unweighted collateral USD = what a liquidator can actually seize against.
        let coll: f64 = a.balances.iter().filter(|b| b.asset_shares > 0.0).filter_map(|b| {
            let bank = banks.get(&b.bank_pk)?;
            let px = prices.get(&b.bank_pk)?;
            let scale = 10f64.powi(bank.mint_decimals as i32);
            Some(b.asset_shares * bank.asset_share_value / scale * px)
        }).sum();
        rows.push(Row { coll, ratio: now.health.ratio(), ratio_shocked: then.health.ratio(),
                        fireable: is_fireable_shape(a, &banks) });
    }

    let bins: [(f64, f64, &str); 5] = [
        (0.0, 0.85, "< 0.85  (safe)"),
        (0.85, 0.95, "0.85 – 0.95"),
        (0.95, 0.97, "0.95 – 0.97"),
        (0.97, 1.00, "0.97 – 1.00  (ARM)"),
        (1.00, f64::INFINITY, "≥ 1.00  (LIQUIDATABLE)"),
    ];
    println!("MARGINFI BORROWER POPULATION — {} priced accounts with debt\n", rows.len());
    println!("{:<24} {:>7} {:>16} {:>10} {:>16}", "health ratio", "accts", "collateral $", "≥ $1k", "fireable coll $");
    println!("{}", "-".repeat(78));
    for (lo, hi, label) in bins {
        let sel: Vec<&Row> = rows.iter().filter(|r| r.ratio >= lo && r.ratio < hi).collect();
        let tot: f64 = sel.iter().map(|r| r.coll).sum();
        let big = sel.iter().filter(|r| r.coll >= 1000.0).count();
        let fire: f64 = sel.iter().filter(|r| r.fireable).map(|r| r.coll).sum();
        println!("{:<24} {:>7} {:>16} {:>10} {:>16}",
            label, sel.len(), format!("{:.0}", tot), big, format!("{:.0}", fire));
    }

    // The money question: a DROP_PCT selloff — what comes into range?
    let newly: Vec<&Row> = rows.iter().filter(|r| r.ratio < 1.0 && r.ratio_shocked >= 1.0).collect();
    let newly_coll: f64 = newly.iter().map(|r| r.coll).sum();
    let newly_fire: f64 = newly.iter().filter(|r| r.fireable).map(|r| r.coll).sum();
    let newly_big = newly.iter().filter(|r| r.coll >= 1000.0).count();
    println!("\n▶ IF EVERY VOLATILE COLLATERAL DROPS {drop_pct}%:");
    println!("   {} accounts newly cross into liquidation range", newly.len());
    println!("   ${:.0} collateral comes into range  (${:.0} of it in our fireable shape)", newly_coll, newly_fire);
    println!("   of those, {} are ≥ $1k positions (worth firing at)", newly_big);

    let mut top: Vec<&Row> = rows.iter().filter(|r| r.ratio >= 0.90).collect();
    top.sort_by(|a, b| b.coll.partial_cmp(&a.coll).unwrap_or(std::cmp::Ordering::Equal));
    println!("\n▶ LARGEST POSITIONS ALREADY WITHIN 10% OF THE THRESHOLD (ratio ≥ 0.90):");
    for r in top.iter().take(12) {
        println!("   ${:>12}  ratio {:.3}  {}", format!("{:.0}", r.coll), r.ratio,
            if r.fireable { "fireable" } else { "SKIP (shape)" });
    }
    let near_total: f64 = top.iter().map(|r| r.coll).sum();
    println!("   → {} accounts, ${:.0} total collateral", top.len(), near_total);
}
