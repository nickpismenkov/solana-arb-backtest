//! Digest of a liquidation executor run — read the JSONL ledgers and summarize
//! so you can answer "is it working / did it earn?" at a glance without tailing
//! the stream. Reads {RUN_DIR}/decisions.jsonl + trades.jsonl (both schemas
//! tolerated). WATCH=1 reprints every REFRESH_SECS.
//!
//! Usage: [RUN_DIR=runs/liq] [WATCH=1] [REFRESH_SECS=30] cargo run --release --bin liq_report

use std::collections::BTreeMap;
use std::io::{BufRead, BufReader};
use std::time::Duration;

fn read_jsonl(path: &str) -> Vec<serde_json::Value> {
    let Ok(f) = std::fs::File::open(path) else { return Vec::new() };
    BufReader::new(f).lines()
        .map_while(Result::ok)
        .filter_map(|l| serde_json::from_str::<serde_json::Value>(&l).ok())
        .collect()
}

fn f(v: &serde_json::Value, k: &str) -> f64 { v.get(k).and_then(|x| x.as_f64()).unwrap_or(0.0) }
fn s<'a>(v: &'a serde_json::Value, k: &str) -> Option<&'a str> { v.get(k).and_then(|x| x.as_str()) }

fn report(run_dir: &str) {
    let decisions = read_jsonl(&format!("{run_dir}/decisions.jsonl"));
    let trades = read_jsonl(&format!("{run_dir}/trades.jsonl"));

    // Liquidation decision rows across ALL executor schemas: marginfi keys the
    // borrower as "liquidatee", Save/Kamino as "obligation" — but all three have
    // "reason" + "fired" (and the arb-engine rows don't), so match on those.
    let liq_decisions: Vec<&serde_json::Value> = decisions.iter()
        .filter(|d| d.get("reason").is_some() && d.get("fired").is_some()).collect();
    let fired = liq_decisions.iter().filter(|d| d.get("fired").and_then(|x| x.as_bool()).unwrap_or(false)).count();
    let mut reasons: BTreeMap<String, usize> = BTreeMap::new();
    for d in &liq_decisions {
        let r = s(d, "reason").unwrap_or("(none)").to_string();
        *reasons.entry(r).or_default() += 1;
    }

    // Trades: liquidation trade rows (have "est_profit_usdc", unlike arb rows).
    // Submissions have a signature; landings have realized_usdc.
    let liq_trades: Vec<&serde_json::Value> = trades.iter()
        .filter(|t| t.get("est_profit_usdc").is_some()).collect();
    let submitted: Vec<&serde_json::Value> = liq_trades.iter().copied()
        .filter(|t| s(t, "signature").is_some()).collect();
    let landed: Vec<&serde_json::Value> = trades.iter()
        .filter(|t| t.get("realized_usdc").map(|x| !x.is_null()).unwrap_or(false)).collect();
    let realized: f64 = landed.iter().map(|t| f(t, "realized_usdc")).sum();
    let errors: Vec<&&serde_json::Value> = submitted.iter()
        .filter(|t| t.get("error").map(|x| !x.is_null()).unwrap_or(false)).collect();

    println!("═══ liquidation report ({run_dir}) ═══");
    println!("decisions logged: {} (liquidation rows)", liq_decisions.len());
    println!("  fired:   {fired}");
    println!("  skipped: {}", liq_decisions.len().saturating_sub(fired));
    if !reasons.is_empty() {
        println!("  reasons:");
        let mut sorted: Vec<(&String, &usize)> = reasons.iter().collect();
        sorted.sort_by_key(|(_, n)| std::cmp::Reverse(**n));
        for (r, n) in sorted.iter().take(12) {
            println!("    {n:>5}  {}", if r.len() > 90 { &r[..90] } else { r });
        }
    }
    println!("trades:");
    println!("  submitted:  {}", submitted.len());
    println!("  errored:    {}", errors.len());
    println!("  landed:     {}", landed.len());
    println!("  realized P&L: ${realized:.2}");
    if landed.is_empty() && submitted.is_empty() {
        println!("\n→ no fires yet. In a calm market that's expected — the bot only fires a");
        println!("  marginfi-confirmed, profitable liquidation. Leave it running.");
    } else if !landed.is_empty() {
        println!("\n→ ★ the strategy has landed real liquidations. That's the money question answered.");
    }
}

fn main() {
    let run_dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs/liq".into());
    let watch = std::env::var("WATCH").map(|s| s == "1").unwrap_or(false);
    let refresh: u64 = std::env::var("REFRESH_SECS").ok().and_then(|s| s.parse().ok()).unwrap_or(30);
    if !watch { report(&run_dir); return; }
    loop {
        print!("\x1b[2J\x1b[H"); // clear screen
        report(&run_dir);
        std::thread::sleep(Duration::from_secs(refresh));
    }
}
