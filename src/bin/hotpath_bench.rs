//! Measure the real hot-path compute reaction (decode → predict → optimal_arb
//! → build_arb_tx → sign → serialize+base64) with live pool data + the real
//! keypair, and separately time a COLD vs WARM Jito connection to show the
//! keep-alive Agent effect. Numbers, not estimates.
//!
//! Usage: RPC_ENDPOINT=<url> ALT_ADDRESS=<alt> KEYPAIR_PATH=<path> cargo run --release --bin hotpath_bench

use arb_engine::arb::{build_arb_tx, load_alt, PoolData};
use arb_engine::clmm::{optimal_arb, wsol, ClmmState};
use arb_engine::jito::{default_block_engine, get_tip_accounts};
use arb_engine::pools::pair;
use base64::Engine;
use solana_hash::Hash;
use solana_keypair::Keypair;
use solana_pubkey::Pubkey;
use solana_signer::Signer;
use std::str::FromStr;
use std::time::Instant;

fn rpc(endpoint: &str, body: serde_json::Value) -> serde_json::Value {
    ureq::post(endpoint).send_json(body).unwrap().into_json().unwrap()
}
fn account_data(endpoint: &str, addr: &str) -> Vec<u8> {
    let v = rpc(endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[addr,{"encoding":"base64"}]}));
    base64::engine::general_purpose::STANDARD.decode(v["result"]["value"]["data"][0].as_str().unwrap()).unwrap()
}

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let endpoint = std::env::var("RPC_ENDPOINT").expect("RPC_ENDPOINT");
    let alt_addr = std::env::var("ALT_ADDRESS").expect("ALT_ADDRESS");
    let kp_path = std::env::var("KEYPAIR_PATH").expect("KEYPAIR_PATH");
    let cfg = pair();
    let base = wsol();

    let bytes: Vec<u8> = serde_json::from_str(&std::fs::read_to_string(&kp_path).unwrap()).unwrap();
    let kp = Keypair::try_from(&bytes[..]).unwrap();
    let signer = kp.pubkey();
    let alt = load_alt(&alt_addr, &account_data(&endpoint, &alt_addr));
    let pd = PoolData { orca: account_data(&endpoint, &cfg.orca_pool), ray: account_data(&endpoint, &cfg.ray_pool) };
    let bh = Hash::from_str(rpc(&endpoint, serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getLatestBlockhash","params":[{"commitment":"confirmed"}]}))["result"]["value"]["blockhash"].as_str().unwrap()).unwrap();

    let orca_mint_a = Pubkey::try_from(&pd.orca[101..133]).ok();
    let (oda, odb) = match orca_mint_a { Some(m) if m == base => (cfg.base_dec, cfg.quote_dec), _ => (cfg.quote_dec, cfg.base_dec) };

    // Warm up.
    let _ = build_arb_tx(&pd, signer, &alt, 500_000_000, true, None, 0, 10_000, bh);

    let n = 2000u32;
    let t0 = Instant::now();
    let mut sink = 0u64;
    for i in 0..n {
        // decode + predict
        let orca0 = ClmmState::from_orca(&pd.orca, oda, odb, cfg.orca_fee_bps).unwrap();
        let ray0 = ClmmState::from_ray(&pd.ray, cfg.ray_fee_bps).unwrap();
        let orca_p = orca0.after_base_swap(&base, i % 2 == 0, 3_000_000_000.0);
        let (size_raw, _profit, buy_orca) = optimal_arb(&orca_p, &ray0, &base, 500.0 * 1e6);
        // build + sign + serialize
        let mut tx = build_arb_tx(&pd, signer, &alt, size_raw.max(1_000_000.0) as u64, buy_orca, None, 0, 10_000, bh).unwrap();
        tx.signatures[0] = kp.sign_message(&tx.message.serialize());
        let b64 = base64::engine::general_purpose::STANDARD.encode(bincode::serialize(&tx).unwrap());
        sink = sink.wrapping_add(b64.len() as u64);
    }
    let per = t0.elapsed().as_secs_f64() * 1e6 / n as f64;
    println!("compute reaction (decode+predict+optimal_arb+build+sign+serialize): {per:.1} µs/iter  (n={n}, sink={sink})");

    // Cold vs warm Jito connection.
    let be = default_block_engine();
    let c0 = Instant::now(); let _ = get_tip_accounts(&be); let cold = c0.elapsed().as_secs_f64() * 1e3;
    let w0 = Instant::now(); let _ = get_tip_accounts(&be); let warm = w0.elapsed().as_secs_f64() * 1e3;
    let w2 = Instant::now(); let _ = get_tip_accounts(&be); let warm2 = w2.elapsed().as_secs_f64() * 1e3;
    println!("Jito round trip: cold(handshake)={cold:.1} ms  warm={warm:.1} ms  warm2={warm2:.1} ms");
    println!("(note: from laptop, not the co-located box — box RTT to Amsterdam is ~0.8ms)");
}
