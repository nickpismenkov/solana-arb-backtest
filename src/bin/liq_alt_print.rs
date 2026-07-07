//! Print the constant accounts of every marginfi-USDC liquidation fire tx —
//! the address set for the dedicated liquidation ALT (LIQ_ALT). Candidate-
//! specific accounts (liquidatee, asset bank/mint/oracle/ATA, Jupiter route)
//! stay static or come via Jupiter's own ALTs.
//!
//! Setup (one-time, ~0.002 SOL reclaimable rent):
//!   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//!   solana address-lookup-table extend <TABLE> --addresses "$(liq_alt_print | paste -sd, -)" …
//!
//! Usage: [HELIUS_RPC=<url>] cargo run --release --bin liq_alt_print

use arb_engine::flashloan::ata_for;
use arb_engine::marginfi;
use solana_pubkey::Pubkey;
use std::str::FromStr;

const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
// USDC bank oracle (Pyth PriceUpdateV2) — cross-checked against the live bank
// when HELIUS_RPC is set.
const USDC_ORACLE: &str = "Dpw1EAVrSB1ibxiDQyTAW6Zip3J4Btk2x4SgApQCeFbX";
const JUPITER_PROGRAM: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";

fn live_usdc_oracle(endpoint: &str) -> Option<Pubkey> {
    use base64::Engine;
    let v: serde_json::Value = ureq::post(endpoint)
        .send_json(serde_json::json!({"jsonrpc":"2.0","id":1,"method":"getAccountInfo",
            "params":[marginfi::USDC_BANK, {"encoding":"base64"}]})).ok()?.into_json().ok()?;
    let raw = base64::engine::general_purpose::STANDARD
        .decode(v["result"]["value"]["data"].get(0)?.as_str()?).ok()?;
    Some(arb_engine::liquidation::Bank::decode(&raw)?.oracle_key)
}

fn main() {
    let _ = dotenvy::dotenv();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let usdc = Pubkey::from_str(marginfi::USDC_MINT).unwrap();
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();
    let token_program = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    let usdc_oracle = match std::env::var("HELIUS_RPC").ok().and_then(|e| live_usdc_oracle(&e)) {
        Some(live) => {
            if live.to_string() != USDC_ORACLE {
                eprintln!("⚠ live USDC oracle {live} differs from constant {USDC_ORACLE} — using live");
            }
            live.to_string()
        }
        None => USDC_ORACLE.into(),
    };

    let addrs = [
        marginfi::MARGINFI_PROGRAM.to_string(),
        marginfi::MARGINFI_GROUP.to_string(),
        marginfi::USDC_BANK.to_string(),
        marginfi::USDC_MINT.to_string(),
        marginfi::bank_liquidity_vault(&usdc_bank).to_string(),
        marginfi::bank_liquidity_vault_auth(&usdc_bank).to_string(),
        marginfi::bank_insurance_vault(&usdc_bank).to_string(),
        usdc_oracle,
        authority.to_string(),
        liquidator_ma.to_string(),
        ata_for(&authority, &usdc, &token_program).to_string(),
        token_program.to_string(),
        "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb".into(), // Token-2022
        "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL".into(), // ATA program
        "11111111111111111111111111111111".into(),            // system
        "ComputeBudget111111111111111111111111111111".into(),
        "Sysvar1nstructions1111111111111111111111111".into(),
        JUPITER_PROGRAM.into(),
    ];
    for a in addrs { println!("{a}"); }
}
