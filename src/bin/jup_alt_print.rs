//! Print the FIXED accounts a Jupiter Lend (Fluid) liquidation fire tx needs in
//! its dedicated address-lookup-table (JUP_ALT), so the marginfi-flash-loan-
//! wrapped `liquidate`+swap+repay tx fits under the 1232-byte single-packet limit.
//! Without an ALT the wrapped tx is ~1723B (see jupiter_fire_probe STAGE 5);
//! moving these ~24 fixed accounts off the static keys (~31B saved each) brings it
//! under 1232, exactly as SAVE_ALT / the Kamino ALT do for their paths.
//!
//! What's FIXED (goes in the ALT) vs per-fire (stays inline / from Jupiter's ALTs):
//!   FIXED  — programs + sysvars, the marginfi USDC flash-loan set, the Fluid
//!            liquidity global PDA, the USDC *borrow-side* per-mint liquidity
//!            accounts (reserve / rate_model / liquidity token vault — identical
//!            for every USDC-debt vault), the Fluid oracle program, and the
//!            wallet + its USDC ATA.
//!   PER-FIRE — vault_config/state, oracle (+ its price sources), the collateral
//!            (supply) mint and its reserve/position/rate_model/token vault, the
//!            vault's borrow position, new_branch, and the tick/branch/tick_has_debt
//!            remaining accounts. These vary per vault; the collateral swap route
//!            rides Jupiter's own ALTs. (A future per-collateral ALT could fold the
//!            common collateral side in too, like the Kamino top-K approach.)
//!
//! Setup (one-time; ALT creation needs wallet signing — do this on the box):
//!   solana address-lookup-table create --keypair ~/arb-keypair.json -u <rpc>
//!   solana address-lookup-table extend <TABLE> --addresses "$(jup_alt_print | paste -sd, -)" …
//! Then export JUP_ALT=<TABLE> for liq_jupiter_executor / jupiter_fire_probe.
//!
//! Usage: [HELIUS_RPC=<url>] [AUTHORITY=<pk>] [LIQUIDATOR_MA=<pk>]
//!        cargo run --release --bin jup_alt_print

use arb_engine::flashloan::ata_for;
use arb_engine::{jupiter, jupiter_math, marginfi};
use solana_pubkey::Pubkey;
use std::str::FromStr;

const DEFAULT_AUTHORITY: &str = "DYeYAvJSKRokeRkjfgLWKyiT9gwvWPVrT2Sa5xYBFSak";
const DEFAULT_LIQUIDATOR_MA: &str = "B6e37TbC5n56tWbcgC3RRafUXSuEwRz9ZbhL8Ksro6vD";
const TOKEN: &str = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA";
const TOKEN22: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
const ATA_PROGRAM: &str = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL";
const SYSTEM: &str = "11111111111111111111111111111111";
const COMPUTE_BUDGET: &str = "ComputeBudget111111111111111111111111111111";
const JUPITER_PROGRAM: &str = "JUP6LkbZbjS1jKKwapdHNy74zcZ3tLUZoi5QNyVTaV4";
// Fluid oracle program (owner of every vault `oracle` account; constant on-chain,
// verified: it is both the `oracle_program` config field and the oracle owner).
const ORACLE_PROGRAM: &str = "jupnw4B6Eqs7ft6rxpzYLJZYSnrpRgPcr589n5Kv4oc";

fn main() {
    let _ = dotenvy::dotenv();
    let _ = rustls::crypto::ring::default_provider().install_default();
    let authority = Pubkey::from_str(&std::env::var("AUTHORITY").unwrap_or_else(|_| DEFAULT_AUTHORITY.into())).unwrap();
    let liquidator_ma = Pubkey::from_str(&std::env::var("LIQUIDATOR_MA").unwrap_or_else(|_| DEFAULT_LIQUIDATOR_MA.into())).unwrap();
    let usdc = Pubkey::from_str(marginfi::USDC_MINT).unwrap();
    let usdc_bank = Pubkey::from_str(marginfi::USDC_BANK).unwrap();
    let token = Pubkey::from_str(TOKEN).unwrap();

    // Fluid liquidity USDC borrow-side (identical for every USDC-debt vault).
    let liquidity = jupiter_math::liquidity_pda();
    let usdc_reserve = jupiter_math::reserve_pda(&usdc);
    let usdc_rate_model = jupiter_math::rate_model_pda(&usdc);
    let usdc_vault_tok_acct = ata_for(&liquidity, &usdc, &token); // vault_borrow_token_account

    // Fluid oracle program: resolve live from any vault's oracle owner if we have
    // RPC (authoritative); else fall back to the known constant.
    let oracle_program = live_oracle_program().unwrap_or_else(|| ORACLE_PROGRAM.into());

    let addrs: Vec<String> = vec![
        // ── programs + sysvars ──
        jupiter::VAULTS_PROGRAM.to_string(),
        jupiter::LIQUIDITY_PROGRAM.to_string(),
        oracle_program,
        JUPITER_PROGRAM.into(),
        marginfi::MARGINFI_PROGRAM.into(),
        TOKEN.into(),
        TOKEN22.into(),
        ATA_PROGRAM.into(),
        SYSTEM.into(),
        COMPUTE_BUDGET.into(),
        // ── marginfi USDC flash-loan fixed set ──
        marginfi::MARGINFI_GROUP.into(),
        marginfi::USDC_BANK.into(),
        marginfi::bank_liquidity_vault(&usdc_bank).to_string(),
        marginfi::bank_liquidity_vault_auth(&usdc_bank).to_string(),
        marginfi::bank_insurance_vault(&usdc_bank).to_string(),
        // ── Fluid liquidity USDC borrow-side (per-mint; same for all USDC vaults) ──
        liquidity.to_string(),
        usdc_reserve.to_string(),
        usdc_rate_model.to_string(),
        usdc_vault_tok_acct.to_string(),
        // ── wallet + USDC ──
        marginfi::USDC_MINT.into(),
        authority.to_string(),
        liquidator_ma.to_string(),
        ata_for(&authority, &usdc, &token).to_string(),
    ];

    // Dedup, preserve order.
    let mut seen = std::collections::HashSet::new();
    let mut n = 0;
    for a in addrs.into_iter().filter(|a| seen.insert(a.clone())) {
        println!("{a}");
        n += 1;
    }
    eprintln!("[jup-alt] {n} fixed accounts. Extend the JUP_ALT with these, then export JUP_ALT=<table>.");
    eprintln!("[jup-alt] liquidity global PDA = {liquidity}");
    eprintln!("[jup-alt] USDC reserve = {usdc_reserve}  rate_model = {usdc_rate_model}  vault_tok_acct = {usdc_vault_tok_acct}");
}

/// If RPC is available, read the oracle program id as the OWNER of any vault's
/// oracle account (ground truth); returns None otherwise.
fn live_oracle_program() -> Option<String> {
    use base64::Engine;
    let endpoint = std::env::var("HELIUS_RPC").or_else(|_| std::env::var("RPC_HTTP")).ok()?;
    // grab one vault config to read its oracle, then read the oracle's owner.
    let disc58 = bs58::encode(jupiter::VAULT_CONFIG_DISC).into_string();
    let v: serde_json::Value = ureq::post(&endpoint).send_json(serde_json::json!({"jsonrpc":"2.0","id":1,
        "method":"getProgramAccounts","params":[jupiter::VAULTS_PROGRAM,
            {"encoding":"base64","dataSlice":{"offset":0,"length":0},
             "filters":[{"memcmp":{"offset":0,"bytes":disc58}}]}]})).ok()?.into_json().ok()?;
    let cfg_pk = v["result"].as_array()?.first()?["pubkey"].as_str()?.to_string();
    let cv: serde_json::Value = ureq::post(&endpoint).send_json(serde_json::json!({"jsonrpc":"2.0","id":1,
        "method":"getAccountInfo","params":[cfg_pk, {"encoding":"base64"}]})).ok()?.into_json().ok()?;
    let raw = base64::engine::general_purpose::STANDARD.decode(cv["result"]["value"]["data"].get(0)?.as_str()?).ok()?;
    let cfg = jupiter::VaultConfig::decode(&raw)?;
    let ov: serde_json::Value = ureq::post(&endpoint).send_json(serde_json::json!({"jsonrpc":"2.0","id":1,
        "method":"getAccountInfo","params":[cfg.oracle.to_string(), {"encoding":"base64"}]})).ok()?.into_json().ok()?;
    ov["result"]["value"]["owner"].as_str().map(String::from)
}
