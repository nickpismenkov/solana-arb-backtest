//! Rollup of the executor ledgers (decisions.jsonl + trades.jsonl): triggers
//! seen, fire rate, realized P&L, tips, per-direction breakdown. Reads the
//! JSONL the executor writes; safe to run while it's live.
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

    let seen = decisions.len();
    let profitable = decisions.iter().filter(|d| d["sim_ok"] == true).count();
    let fired = trades.iter().filter(|t| t["bundle_id"].is_string()).count();
    let resolved: Vec<&serde_json::Value> = trades.iter().filter(|t| t["realized_usdc"].is_number()).collect();
    let realized: f64 = resolved.iter().filter_map(|t| t["realized_usdc"].as_f64()).sum();
    let tips_sol: f64 = trades.iter().filter(|t| t["bundle_id"].is_string()).filter_map(|t| t["tip_lamports"].as_f64()).sum::<f64>() / 1e9;
    let errors = trades.iter().filter(|t| t["error"].is_string()).count();

    println!("\n──────── arb executor rollup ({dir}) ────────");
    println!("triggers evaluated:  {seen}   (profitable sims: {profitable})");
    println!("bundles fired:       {fired}   submit errors: {errors}");
    println!("trades w/ realized:  {}   realized P&L: ${:.4} USDC", resolved.len(), realized);
    println!("tips paid:           {tips_sol:.6} SOL");

    let dir_of = |d: &str| -> (usize, usize) {
        let s = decisions.iter().filter(|x| x["dir"] == d && x["sim_ok"] == true).count();
        let f = trades.iter().filter(|x| x["dir"] == d && x["bundle_id"].is_string()).count();
        (s, f)
    };
    for d in ["orca->ray", "ray->orca"] {
        let (s, f) = dir_of(d);
        println!("  {d:10}  profitable sims: {s}  fired: {f}");
    }
    println!();
}
