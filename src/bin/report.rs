//! Rollup of the executor ledgers (decisions.jsonl + trades.jsonl): decodable
//! victims evaluated, profitable predictions, fires, landings, realized P&L,
//! tips paid. Reads the JSONL the executor writes; safe to run while it's live.
//!
//! Usage: RUN_DIR=runs cargo run --release --bin report

use std::io::BufRead;

fn read_jsonl(path: &str) -> Vec<serde_json::Value> {
    let Ok(f) = std::fs::File::open(path) else { return vec![] };
    std::io::BufReader::new(f)
        .lines()
        .map_while(Result::ok)
        .filter_map(|l| serde_json::from_str(&l).ok())
        .collect()
}

fn main() {
    let dir = std::env::var("RUN_DIR").unwrap_or_else(|_| "runs".into());
    let decisions = read_jsonl(&format!("{dir}/decisions.jsonl"));
    let trades = read_jsonl(&format!("{dir}/trades.jsonl"));

    // Decisions: one per decodable victim we evaluated (routed/CPI skipped).
    let evaluated = decisions.len();
    let profitable = decisions.iter().filter(|d| d["reason"] == "profitable").count();
    let below = decisions.iter().filter(|d| d["reason"] == "below_threshold").count();
    let fired = decisions.iter().filter(|d| d["fired"] == true).count(); // live submits (0 in dry run)

    // Trades: submit errors, and confirmed on-chain landings (realized_usdc set).
    let submit_errors = trades.iter().filter(|t| t["error"].is_string()).count();
    let landed: Vec<&serde_json::Value> = trades.iter().filter(|t| t["realized_usdc"].is_number()).collect();
    let realized: f64 = landed.iter().filter_map(|t| t["realized_usdc"].as_f64()).sum();
    let tips_sol: f64 = landed.iter().filter_map(|t| t["tip_lamports"].as_f64()).sum::<f64>() / 1e9;

    println!("\n──────── executor rollup ({dir}) ────────");
    println!("decodable victims evaluated:  {evaluated}");
    println!("  predicted PROFITABLE:       {profitable}   (below threshold: {below})");
    println!("  fired live:                 {fired}   (0 in dry run)");
    println!("submit errors:                {submit_errors}");
    println!("LANDED on-chain:              {}", landed.len());
    println!("realized P&L:                 {realized:+.4} USDC");
    println!("tips paid (landed only):      {tips_sol:.6} SOL");
    if landed.is_empty() && profitable > 0 {
        println!("\n→ found {profitable} profitable predictions but nothing landed = losing the race (or dry run).");
    } else if profitable == 0 {
        println!("\n→ no profitable predictions = no capturable edge in this window (or market quiet).");
    }
    println!();
}
