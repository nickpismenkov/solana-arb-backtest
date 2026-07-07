//! Liquidation opportunity probe (read-only). For each lending/perps protocol,
//! scan recent program transactions, identify LIQUIDATIONS (by instruction
//! discriminator at top-level or via CPI, or by a log marker), and measure:
//! frequency (→ liquidations/day), liquidator concentration (are a few bots
//! winning everything?), and a rough profit proxy (fee-payer USDC delta). Tells
//! us which protocols are worth building an adapter for, before we build one.
//!
//! Discriminators/program IDs are VERIFIED per protocol before trust (marginfi
//! computed; others pending research). Read-only, no money.
//!
//! Usage: RPC_ENDPOINT=<helius> [LIMIT=1000] [SLEEP_MS=25] cargo run --release --bin liq_probe

use std::collections::HashMap;
use std::time::Duration;

const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";

struct Protocol {
    name: &'static str,
    program: &'static str,
    discs: &'static [[u8; 8]], // Anchor liquidation ix discriminators
    tags: &'static [u8],       // non-Anchor: match instruction data[0]
    log_markers: &'static [&'static str], // fallback: log substrings
}

// Verified program IDs + discriminators (research + local sha256). Profit note:
// Kamino/Solend expose liquidator gain in token balances; marginfi/Drift keep it
// as internal share/margin deltas → our USDC-delta proxy under-reads those (the
// reliable signals there are frequency + liquidator concentration).
fn protocols() -> Vec<Protocol> {
    vec![
        Protocol {
            name: "kamino-klend",
            program: "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD",
            discs: &[[177, 71, 154, 188, 226, 133, 74, 55], [162, 161, 35, 143, 30, 187, 185, 103]],
            tags: &[],
            log_markers: &["LiquidateObligationAndRedeemReserveCollateral"],
        },
        Protocol {
            name: "marginfi-v2",
            program: "MFv2hWf31Z9kbCa1snEPYctwafyhdvnV7FZnsebVacA",
            discs: &[[214, 169, 151, 213, 251, 167, 86, 219]],
            tags: &[],
            log_markers: &["LendingAccountLiquidate"],
        },
        Protocol {
            name: "drift-v2",
            program: "dRiftyHA39MWEi3m9aunc5MzRF1JYuBsbn6VPcn33UH",
            discs: &[
                [75, 35, 119, 247, 191, 18, 139, 2],    // liquidate_perp
                [107, 0, 128, 41, 35, 229, 251, 18],    // liquidate_spot
                [95, 111, 124, 105, 86, 169, 187, 34],  // liquidate_perp_with_fill
            ],
            tags: &[],
            log_markers: &[],
        },
        Protocol {
            name: "save-solend",
            program: "So1endDq2YkqhipRh3WViPa8hdiSpxWy6z3Z6tMCpAo",
            discs: &[],
            tags: &[12, 17], // LiquidateObligation / …AndRedeemReserveCollateral
            log_markers: &["LiquidateObligation"],
        },
    ]
}

fn rpc(endpoint: &str, body: serde_json::Value) -> Option<serde_json::Value> {
    for attempt in 0..4 {
        if let Ok(resp) = ureq::post(endpoint).send_json(body.clone()) {
            if let Ok(v) = resp.into_json::<serde_json::Value>() { return Some(v); }
        }
        std::thread::sleep(Duration::from_millis(300 << attempt));
    }
    None
}

fn recent_sigs(endpoint: &str, program: &str, limit: u32) -> Vec<String> {
    rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getSignaturesForAddress","params":[program,{"limit":limit}]}))
        .and_then(|v| v["result"].as_array().cloned()).unwrap_or_default()
        .iter().filter(|e| e["err"].is_null())
        .filter_map(|e| e["signature"].as_str().map(String::from)).collect()
}

fn disc8(b58: &str) -> Option<[u8; 8]> {
    let bytes = bs58::decode(b58).into_vec().ok()?;
    if bytes.len() < 8 { return None; }
    let mut d = [0u8; 8];
    d.copy_from_slice(&bytes[..8]);
    Some(d)
}

fn pct(sorted: &[f64], p: f64) -> f64 {
    if sorted.is_empty() { return 0.0; }
    sorted[(((sorted.len() - 1) as f64) * p).round() as usize]
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT (use Helius)");
    let limit: u32 = std::env::var("LIMIT").ok().and_then(|s| s.parse().ok()).unwrap_or(1000);
    let sleep_ms: u64 = std::env::var("SLEEP_MS").ok().and_then(|s| s.parse().ok()).unwrap_or(25);

    for p in protocols() {
        eprintln!("\n═══ {} ({}) — scanning {limit} recent program txns ═══", p.name, &p.program[..8]);
        let sigs = recent_sigs(&endpoint, p.program, limit);
        if sigs.is_empty() { println!("  no signatures (program not found / RPC issue)"); continue; }

        let (mut liqs, mut min_t, mut max_t) = (0u64, u64::MAX, 0u64);
        let mut by_liquidator: HashMap<String, u64> = HashMap::new();
        let mut profits: Vec<f64> = Vec::new();
        let mut scanned = 0u64;

        for sig in &sigs {
            std::thread::sleep(Duration::from_millis(sleep_ms));
            let Some(v) = rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getTransaction",
                "params":[sig,{"encoding":"jsonParsed","maxSupportedTransactionVersion":0,"commitment":"confirmed"}]})) else { continue };
            let r = &v["result"];
            if r.is_null() || !r["meta"]["err"].is_null() { continue; }
            scanned += 1;

            // Is this a liquidation? Check top-level + inner instructions for our
            // program + a liquidation discriminator, or a log marker.
            let mut is_liq = false;
            let check_ix = |ix: &serde_json::Value| -> bool {
                if ix["programId"].as_str() != Some(p.program) { return false; }
                let Some(data) = ix["data"].as_str() else { return false };
                // Anchor: 8-byte discriminator. Non-Anchor (Solend): data[0] tag.
                if let Some(d) = disc8(data) { if p.discs.contains(&d) { return true; } }
                if let Some(b0) = bs58::decode(data).into_vec().ok().and_then(|v| v.first().copied()) {
                    if p.tags.contains(&b0) { return true; }
                }
                false
            };
            for ix in r["transaction"]["message"]["instructions"].as_array().into_iter().flatten() {
                if check_ix(ix) { is_liq = true; break; }
            }
            if !is_liq {
                for grp in r["meta"]["innerInstructions"].as_array().into_iter().flatten() {
                    for ix in grp["instructions"].as_array().into_iter().flatten() {
                        if check_ix(ix) { is_liq = true; break; }
                    }
                }
            }
            if !is_liq && !p.log_markers.is_empty() {
                let logs = r["meta"]["logMessages"].as_array().map(|a| a.iter().filter_map(|l| l.as_str()).collect::<Vec<_>>().join("\n")).unwrap_or_default();
                if p.log_markers.iter().any(|m| logs.contains(m)) { is_liq = true; }
            }
            if !is_liq { continue; }

            liqs += 1;
            if let Some(t) = r["blockTime"].as_u64() { min_t = min_t.min(t); max_t = max_t.max(t); }
            let payer = r["transaction"]["message"]["accountKeys"][0]["pubkey"].as_str().unwrap_or("").to_string();
            *by_liquidator.entry(payer.clone()).or_insert(0) += 1;
            // Rough profit proxy: fee-payer USDC delta.
            let sum = |key: &str| -> f64 {
                r["meta"][key].as_array().into_iter().flatten()
                    .filter(|b| b["mint"] == USDC && b["owner"] == payer.as_str())
                    .filter_map(|b| b["uiTokenAmount"]["uiAmount"].as_f64()).sum()
            };
            profits.push(sum("postTokenBalances") - sum("preTokenBalances"));
        }

        let span_h = if max_t > min_t { (max_t - min_t) as f64 / 3600.0 } else { 0.0 };
        let per_day = if span_h > 0.0 { liqs as f64 / span_h * 24.0 } else { 0.0 };
        println!("  scanned {scanned} txns → {liqs} liquidations over {span_h:.1}h  (~{per_day:.0}/day)");
        if liqs == 0 {
            println!("  → none found in window (disc/marker may need fixing, or genuinely rare here)");
            continue;
        }
        // Liquidator concentration.
        let mut top: Vec<(&String, &u64)> = by_liquidator.iter().collect();
        top.sort_by(|a, b| b.1.cmp(a.1));
        let total: u64 = by_liquidator.values().sum();
        println!("  distinct liquidators: {}  | top 3 share:", by_liquidator.len());
        for (who, n) in top.iter().take(3) {
            println!("    {}… {} ({:.0}%)", &who[..8.min(who.len())], n, 100.0 * **n as f64 / total as f64);
        }
        profits.retain(|x| *x > 0.0);
        profits.sort_by(|a, b| a.partial_cmp(b).unwrap());
        if !profits.is_empty() {
            println!("  liquidator USDC gain (rough, n={}): med ${:.2}  p90 ${:.2}  max ${:.2}",
                profits.len(), pct(&profits, 0.5), pct(&profits, 0.9), pct(&profits, 1.0));
        }
    }
    println!();
}
