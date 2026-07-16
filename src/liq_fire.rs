//! The atomic liquidation FIRE path — one flashloan-wrapped v0 tx:
//!
//!   [cu_limit, cu_price, create ATAs, start_flashloan,
//!    liquidate → withdraw_all seized collateral → Jupiter swap collateral→USDC
//!    → repay_all liability, end_flashloan, tip]
//!
//! Profit-or-revert with NO external capital: `liquidate` moves internal shares
//! (liquidator gains asset shares + takes on the matching liability), so no
//! tokens are needed up front; `repay_all` fails unless the swap produced
//! enough USDC to cover the full liability, and `end_flashloan` re-checks
//! account health — either the whole tx lands net-positive (surplus USDC stays
//! in the wallet ATA) or it reverts and costs nothing but the fee on a miss.
//!
//! v1 restriction: the liability bank must be USDC (the dominant marginfi debt
//! asset) — the swap leg targets USDC and `payback_usdc` closes it.

use crate::arb::{cu_limit_ix, cu_price_ix, transfer_ix};
use crate::flashloan::{ata_for, create_ata_idempotent_for};
use crate::jup;
use crate::marginfi;
use crate::{arb, clmm};
use crate::swap::{orca_swap_ix, orca_swap_v2_ix, sqrt_limit};
use anyhow::{anyhow, Result};
use solana_hash::Hash;
use solana_instruction::AccountMeta;
use solana_message::{v0, VersionedMessage};
use solana_pubkey::Pubkey;
use solana_transaction::versioned::VersionedTransaction;
use std::str::FromStr;

pub const FIRE_CU_LIMIT: u32 = 1_400_000;

/// Dedicated ALT holding the 18 accounts common to every marginfi-USDC
/// liquidation (see liq_alt_print for the set + recreate instructions).
/// Override via LIQ_ALT.
pub const LIQ_ALT: &str = "DEMhLvSJbSZQfCdiH7YicYNopo3EhhapjfoEjt2kJVij";

/// Everything the executor knows about one liquidation opportunity.
pub struct FireCandidate {
    pub liquidatee: Pubkey,
    pub asset_bank: Pubkey,
    pub asset_mint: Pubkey,
    pub asset_token_program: Pubkey,
    /// Collateral native units to seize (sized by the caller via simulation).
    pub asset_amount: u64,
    /// The liability (debt) bank the liquidator absorbs and must repay. Any of
    /// USDC/USDT/wSOL in v1.5.
    pub liab_bank: Pubkey,
    /// The debt asset's mint + token program — the swap target and payback token
    /// (was hardcoded USDC; now the actual absorbed-liability asset).
    pub debt_mint: Pubkey,
    pub debt_token_program: Pubkey,
    pub asset_oracle: Pubkey,
    pub liab_oracle: Pubkey,
    /// The liquidatee's observation list: [bank(ro), oracle(ro)] per active
    /// balance, in balance order.
    pub liquidatee_obs: Vec<AccountMeta>,
}

pub struct FireTx {
    /// Unsigned (sign before sending; default signature placeholder).
    pub tx: VersionedTransaction,
    /// Jupiter's quoted DEBT-asset out (native) for the seized collateral —
    /// compare against the absorbed liability to decide whether firing is worth
    /// it. (Named `quoted_usdc_out` historically; now the debt asset, which may
    /// be USDC/USDT/wSOL.)
    pub quoted_usdc_out: u64,
    pub tx_bytes: usize,
}

/// Build the unsigned fire tx. Quotes the collateral→USDC swap live (Jupiter),
/// so call this only for a sim-confirmed candidate. `blockhash` = real recent
/// hash for live submission, or default for replace-blockhash simulation.
/// Shared, live pool-state cache. The streaming executor gRPC-subscribes the DEX
/// pools and pushes their bytes here, so the fire path reads pool state from RAM
/// instead of a ~45ms getAccountInfo — critical for the burst "tail" (fires that
/// weren't pre-armed). Empty by default → falls back to RPC (polling executor).
static POOL_CACHE: std::sync::OnceLock<std::sync::RwLock<std::collections::HashMap<Pubkey, Vec<u8>>>> = std::sync::OnceLock::new();
fn pool_cache() -> &'static std::sync::RwLock<std::collections::HashMap<Pubkey, Vec<u8>>> {
    POOL_CACHE.get_or_init(|| std::sync::RwLock::new(std::collections::HashMap::new()))
}
/// Push fresh pool bytes (called from the gRPC stream on each pool update).
pub fn update_pool_cache(pool: Pubkey, bytes: Vec<u8>) { pool_cache().write().unwrap().insert(pool, bytes); }
/// The DEX pool addresses to subscribe/stream (so the executor knows what to watch).
pub fn dex_pool_addresses() -> Vec<Pubkey> {
    let mut seen = std::collections::HashSet::new();
    DEX_POOLS.iter()
        .filter_map(|(_, _, p)| Pubkey::from_str(p).ok())
        .filter(|p| seen.insert(*p))
        .collect()
}

/// Direct-DEX routes for the collateral→debt swap (bypasses Jupiter/lite-api,
/// which is rate-limited to death). Orca Whirlpool only for now.
/// (collateral mint, debt mint, deepest Orca Whirlpool for the pair) — pools
/// discovered on-chain (deepest by liquidity across both mint orderings; see
/// scratchpad discover_pools_multi.py). Debt legs: USDC, USDT, wSOL — the full
/// wired debt scope. The pre-arm sim-gate rejects any entry that doesn't
/// build/sim cleanly, so a wrong/thin pool is harmless — it just never fires.
const USDC: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
const USDT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
const WSOL: &str = "So11111111111111111111111111111111111111112";
const DEX_POOLS: &[(&str, &str, &str)] = &[
    // → USDC (sim-verified in production — do not churn)
    ("DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", USDC, "5P6n5omLbLbP4kaPGL8etqQAHEx2UCkaUyvjLDnwV4EY"), // BONK
    ("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", USDC, "AU971DrPyhhrpRnmEBp5pDTWL2ny7nofb5vYBjDJkR2E"),
    ("85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ", USDC, "91E61RiGhH9b9Ns8wrb4E3oBNdtkQx2k4xb33pSqt5am"),
    ("HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", USDC, "Fra9rBL1F5eAgtoqjXsBzZocD1UKbxXoERKVs6e23ixn"),
    ("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", USDC, "9tXiuRRw7kbejLhZXtxDxYs2REe43uH2e7k1kocgdM9B"),
    ("EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", USDC, "CN8M75cH57DuZNzW5wSUpTXtMrSfXBFScJoQxVCgAXes"),
    ("pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn", USDC, "4AFAkCSkSNmra64irggEFd8ZtF4WCtFe51qVaFFNBL2D"), // PUMP
    ("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", USDC, "55BrDTCLWayM16GwrMEQU57o4PTm6ceF9wavSdNZcEiy"),
    ("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", USDC, "AxqAWNZqozhTn2pkDPgpf5kc5DeBuhLKKNWnt3dLrxdi"), // USDS
    ("JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN", USDC, "4Ui9QdDNuUaAGqCPcDSp191QrixLzQiLxJ1Gnqvz3szP"), // JUP
    ("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", USDC, "9RqDTfwCx2SgxsvKpspQHc38HUo3B6hRd3oR9JR966Ps"),
    ("27G8MtK7VtTcCHkpASjSDdkWWYfoqT6ggEuKidVJidD4", USDC, "HD8i7qr1hd9ida6sN71RbkLxbWcbvZS4NA5CY6vfcDpj"),
    ("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", USDC, "HxA6SKW5qA4o12fjVgTpXdq2YnZ5Zv1s7SB4FFomsyLM"), // cbBTC
    ("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", USDC, "3wijQvPKm6jHQrAkfPpok5o8WjCWPm1DGG17NmeW8q1w"), // CASH
    ("hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux", USDC, "5LnAsMfjG32kdUauAzEuzANT6YmM3TSRpL1rWsCUDKus"), // HNT
    // → wSOL (every collateral has a deep SOL leg; BONK/SOL is 37× the USDC pool)
    ("DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263", WSOL, "5zpyutJu9ee6jFymDGoK7F6S5Kczqtc9FomP3ueKuyA9"), // BONK
    ("7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", WSOL, "HktfL7iwGKT5QHjywQkcDnZXScoh811k7akrMZJkCcEF"),
    ("85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ", WSOL, "CwHuXNNkj5inuj2ZXaU1DtjA5Nxfoiy4nNoc1PQQJxTR"),
    ("HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3", WSOL, "8erNF5u3CHrqZJXtkfY8CjSxFYF1yqHmN8uDbAhk6tWM"),
    ("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", WSOL, "6Wfzz7Xczn4pciH4LnvF79r34htiWpTXNPCFz4jWZpi3"),
    ("EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm", WSOL, "D6NdKrKNQPmRZCCnG1GqXtF7MMoHB7qR6GU5TkG59Qz1"),
    ("pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn", WSOL, "BofA2ViUSudPBTUms2KRuG6AHNeMawjNfwqTJDgx5BKW"), // PUMP
    ("3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", WSOL, "B5EwJVDuAauzUEEdwvbuXzbFFgEYnUqqS37TUM1c4PQA"),
    ("USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA", WSOL, "3z5mw25EoNssdjBvdXrikM32AyaNdaaC3cohBGCvHTYB"), // USDS (thin)
    ("JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN", WSOL, "C1MgLojNLWBKADvu9BHdtgzz1oZX4dZ5zGdGcgvvW8Wz"), // JUP
    ("2u1tszSeqZ3qBWF3uNGPFc8TzMk2tdiwknnRMWGWjGWH", WSOL, "5KqohoeGjTjyHAFJJywK4J7fkFuK82PfMyuseGgLKZu2"),
    ("27G8MtK7VtTcCHkpASjSDdkWWYfoqT6ggEuKidVJidD4", WSOL, "6a3m2EgFFKfsFuQtP4LJJXPcAe3TQYXNyHUjjZpUxYgd"),
    ("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", WSOL, "CeaZcxBNLpJWtxzt58qQmfMBtJY8pQLvursXTJYGQpbN"), // cbBTC
    ("CASHx9KJUStyftLFWGvEVf59SGeG9sh5FfcnZMVPCASH", WSOL, "FmXTEwVEy8cjoApSeBfbVgqtp8ysZ5GWoKhcbWjTNXBi"), // CASH
    ("hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux", WSOL, "CSP4RmB6kBHkKGkyTnzt9zYYXDA8SbZ5Do5WfZcjqjE4"), // HNT
    // → USDT (only pairs with a live pool; thin ones are sim-gated anyway)
    ("2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo", USDT, "39GrsozbzM9Sg1U7EDnEtQ69fsVF3pmVtmV2DGDAQQJ5"),
    ("pumpCmXqMfrsAkQ5r49WcJnRayYRqmXz6ae8H7H9Dfn", USDT, "5fK65u2QzSynMuRTbovUZq1qgpUSmE35jUBAXziBzpM"), // PUMP (thin)
    ("JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN", USDT, "BJaSQNjHug4jYdTnNeD12UVzUvxh5CEKuP74YQ2B7RXv"), // JUP (thin)
    ("cbbtcf3aa214zXHbiAZQwf4122FBYbraNdFqgw4iMij", USDT, "EDFqcpfDt8eZgwcmMCRTJmG62k7VnnVNx7XbeQ1Hv5p5"), // cbBTC (thin)
    // stable/SOL collateral ↔ stable/SOL debt (stable collateral liqs when the debt asset pumps)
    (USDC, USDT, "4fuUiYxTQ6QCrdSq9ouBYcTM7bqSwYTSyLueGZLTy4T4"),
    (USDC, WSOL, "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
    (USDT, USDC, "4fuUiYxTQ6QCrdSq9ouBYcTM7bqSwYTSyLueGZLTy4T4"),
    (USDT, WSOL, "FwewVm8u6tFPGewAyHmWAqad9hmF7mvqxK4mJ7iNqqGC"),
    (WSOL, USDC, "Czfq3xZZDmsdGdUyrNLtRhGc47cXcZtLG4crryfu44zE"),
    (WSOL, USDT, "FwewVm8u6tFPGewAyHmWAqad9hmF7mvqxK4mJ7iNqqGC"),
    // LST / USDS pair legs (deepest on-chain) — these serve both as collateral
    // routes and as the second hop of exotic-DEBT routes (e.g. BONK→SOL→JitoSOL);
    // pool_for_pair looks the table up in either direction.
    ("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", WSOL, "Hp53XEtt4S8SvPCXarsLSdGfZBuUr5mMmZmX2DRNXQKp"), // JitoSOL
    ("J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn", USDC, "5hWJUNTtEtKmKgDXpthJXXRRmJrz5vJ7uJzrUNVdrwLg"),
    ("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", WSOL, "EkDQqNFijZHQDKfM8KcNBFb3UUGaJRuZpJkj1Zu2BYFN"), // mSOL
    ("mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So", USDC, "AqJ5JYNb7ApkJwvbuXxPnTtKeuizjvC1s2fkp382y9LC"),
    ("LSTxxxnJzKDFSLr4dUkPcmCf5VyryEqzPLz5j4bpxFp", WSOL, "HJVNnnRj1xz25P9215AHQUvGXoS6MKtJASjgrrwD7GnP"), // LST
    ("bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1", WSOL, "8phK65jxmTPEN158xLgSr4oZvssw9SyTErpNZj3g7px4"), // bSOL
];

pub fn direct_dex_pool(collateral: &Pubkey, debt: &Pubkey) -> Option<Pubkey> {
    let (c, d) = (collateral.to_string(), debt.to_string());
    DEX_POOLS.iter().find(|(m, dm, _)| *m == c && *dm == d).and_then(|(_, _, p)| Pubkey::from_str(p).ok())
}

/// A Whirlpool swaps both directions — look the pair up either way.
fn pool_for_pair(a: &Pubkey, b: &Pubkey) -> Option<Pubkey> {
    direct_dex_pool(a, b).or_else(|| direct_dex_pool(b, a))
}

/// Two-hop route via wSOL or USDC when no direct pool exists (e.g. BONK→USDT =
/// BONK→SOL→USDT; BONK→JitoSOL = BONK→SOL→JitoSOL). Returns (pool1, mid, pool2).
pub fn two_hop_route(collateral: &Pubkey, debt: &Pubkey) -> Option<(Pubkey, Pubkey, Pubkey)> {
    for mid in [WSOL, USDC] {
        let midpk = Pubkey::from_str(mid).ok()?;
        if *collateral == midpk || *debt == midpk { continue; }
        if let (Some(p1), Some(p2)) = (pool_for_pair(collateral, &midpk), pool_for_pair(&midpk, debt)) {
            return Some((p1, midpk, p2));
        }
    }
    None
}

/// Whether the fire path can swap collateral→debt at all (same mint, direct
/// pool, or two-hop). The executor's leg-picker uses this to choose pairs.
pub fn swap_route_exists(collateral: &Pubkey, debt: &Pubkey) -> bool {
    collateral == debt || direct_dex_pool(collateral, debt).is_some() || two_hop_route(collateral, debt).is_some()
}

/// Whether a collateral mint has ANY direct-DEX route (any debt leg) — used by
/// the executor's arm-dedupe (coverable collateral gets unlimited arm slots).
pub fn has_direct_dex(collateral: &Pubkey) -> bool {
    let c = collateral.to_string();
    DEX_POOLS.iter().any(|(m, _, _)| *m == c)
}

/// Fetch one account's raw bytes via RPC (off the hot path) — live pool state for
/// the direct-DEX swap (tick arrays + price).
fn fetch_account(endpoint: &str, key: &Pubkey) -> Result<Vec<u8>> {
    use base64::Engine;
    let v: serde_json::Value = ureq::post(endpoint).send_json(serde_json::json!(
        {"jsonrpc":"2.0","id":1,"method":"getAccountInfo","params":[key.to_string(), {"encoding":"base64"}]}))?
        .into_json()?;
    let d = v["result"]["value"]["data"].get(0).and_then(|x| x.as_str())
        .ok_or_else(|| anyhow!("no account data for {key}"))?;
    Ok(base64::engine::general_purpose::STANDARD.decode(d)?)
}

/// Build ONE Orca Whirlpool swap ix (in_mint → the pool's other mint) entirely
/// from live pool bytes (no network aggregator). Returns (ix, quoted_out).
fn orca_swap_leg(rpc_endpoint: &str, pool_pk: Pubkey, authority: &Pubkey,
    in_mint: Pubkey, in_tp: Pubkey, out_tp: Pubkey,
    swap_in: u64, slippage_bps: u32) -> Result<(solana_instruction::Instruction, u64)> {
    // Streamed pool state from RAM (µs) if present; else RPC fetch (~45ms fallback).
    let pb = match pool_cache().read().unwrap().get(&pool_pk) {
        Some(b) => b.clone(),
        None => fetch_account(rpc_endpoint, &pool_pk)?,
    };
    if pb.len() < 213 { return Err(anyhow!("orca pool too small")); }
    // Direction: input is token0 (a_to_b / zero_for_one) iff in_mint == mint0@101.
    let mint0 = arb::pk_at(&pb, 101);
    let a_to_b = in_mint == mint0;
    let fee_rate = u16::from_le_bytes([pb[45], pb[46]]) as f64; // Orca feeRate (1e-6) → bps = /100
    let cl = clmm::ClmmState::from_orca(&pb, 0, 0, fee_rate / 100.0).ok_or_else(|| anyhow!("orca state"))?;
    let quoted = cl.apply_swap(a_to_b, swap_in as f64).max(0.0) as u64;
    let min_out = (quoted as f64 * (1.0 - slippage_bps as f64 / 1e4)) as u64;
    let accts = arb::orca_accounts(&pb, pool_pk, *authority, a_to_b, in_mint, in_tp, out_tp);
    // Token-2022 mints (e.g. cbBTC) need swap_v2 (passes token programs + mints).
    const T22: &str = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb";
    let needs_v2 = in_tp.to_string() == T22 || out_tp.to_string() == T22;
    let ix = if needs_v2 {
        let mint_a = arb::pk_at(&pb, 101);
        let mint_b = arb::pk_at(&pb, 181);
        let (tp_a, tp_b) = if a_to_b { (in_tp, out_tp) } else { (out_tp, in_tp) };
        orca_swap_v2_ix(&accts, mint_a, mint_b, tp_a, tp_b, swap_in, min_out, sqrt_limit(a_to_b), true, a_to_b)
    } else {
        orca_swap_ix(&accts, swap_in, min_out, sqrt_limit(a_to_b), true, a_to_b)
    };
    Ok((ix, quoted))
}

/// Swap ix(s) for the seized collateral → debt asset. Returns (ixs, quoted_out).
fn orca_direct_swap(rpc_endpoint: &str, pool_pk: Pubkey, c: &FireCandidate, authority: &Pubkey,
    swap_in: u64, slippage_bps: u32) -> Result<(Vec<solana_instruction::Instruction>, u64)> {
    let (ix, quoted) = orca_swap_leg(rpc_endpoint, pool_pk, authority,
        c.asset_mint, c.asset_token_program, c.debt_token_program, swap_in, slippage_bps)?;
    Ok((vec![ix], quoted))
}

#[allow(clippy::too_many_arguments)]
pub fn build_fire_tx(
    rpc_endpoint: &str,
    c: &FireCandidate,
    liquidator_ma: &Pubkey,
    authority: &Pubkey,
    tip_account: Option<Pubkey>,
    tip_lamports: u64,
    priority_micro_lamports: u64,
    slippage_bps: u32,
    max_swap_accounts: usize,
    blockhash: Hash,
) -> Result<FireTx> {
    let token_program = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();

    // Swap leg: ExactIn the seized collateral → DEBT asset (USDC/USDT/wSOL).
    // Haircut 0.05%: the seize→withdraw round-trip goes through marginfi share
    // math and can round down a few native units, and an ExactIn of the full
    // amount would fail on insufficient funds — the dust stays in the asset ATA.
    // wrap_sol=false — a SOL collateral withdraw lands wSOL in the wSOL ATA, and
    // a wSOL-debt swap output also lands as wSOL, which payback spends directly.
    //
    // Same-mint case (collateral mint == debt mint, e.g. SOL collateral / SOL
    // debt): no swap — the withdrawn collateral IS the debt asset (same ATA), so
    // repay spends it directly. Jupiter rejects equal in/out mints, so we must
    // skip the quote entirely. quoted_out ≈ the withdrawn amount.
    let swap_in = c.asset_amount.saturating_sub(c.asset_amount / 2000 + 1);
    let same_mint = c.asset_mint == c.debt_mint;
    // mid_ata: for a two-hop route the intermediate mint (wSOL/USDC) needs its ATA
    // created idempotently too — hop-1 output lands there before hop-2 spends it.
    let mut mid_ata_ix: Option<solana_instruction::Instruction> = None;
    let (swap_ixs, quoted_out, swap_alts): (Vec<_>, u64, Vec<Pubkey>) = if same_mint {
        (Vec::new(), swap_in, Vec::new())
    } else if let Some(pool) = direct_dex_pool(&c.asset_mint, &c.debt_mint) {
        // Direct-DEX (Orca Whirlpool) — no Jupiter, no HTTP quote, no rate limit.
        let (ixs, quoted) = orca_direct_swap(rpc_endpoint, pool, c, authority, swap_in, slippage_bps)?;
        (ixs, quoted, Vec::new())
    } else if let Some((pool1, mid, pool2)) = two_hop_route(&c.asset_mint, &c.debt_mint) {
        // Two-hop via wSOL/USDC (both classic Token mints). Hop-2's exact-in is
        // hop-1's MIN out: hop-1 either delivers ≥ that (surplus stays as wSOL/
        // USDC dust in our ATA — value kept) or fails the whole tx atomically.
        let mid_tp = Pubkey::from_str("TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA").unwrap();
        let (ix1, out1) = orca_swap_leg(rpc_endpoint, pool1, authority,
            c.asset_mint, c.asset_token_program, mid_tp, swap_in, slippage_bps)?;
        let min1 = (out1 as f64 * (1.0 - slippage_bps as f64 / 1e4)) as u64;
        if min1 == 0 { return Err(anyhow!("two-hop {}→{}: zero mid quote", c.asset_mint, c.debt_mint)); }
        let (ix2, out2) = orca_swap_leg(rpc_endpoint, pool2, authority,
            mid, mid_tp, c.debt_token_program, min1, slippage_bps)?;
        mid_ata_ix = Some(create_ata_idempotent_for(authority, &mid, &mid_tp));
        (vec![ix1, ix2], out2, Vec::new())
    } else {
        // NO Jupiter on the fire path. A Jupiter HTTP quote is 100s of ms (+429
        // backoff) — it can never be a 1ms fire, and its multi-second hangs starve
        // the MAX_INFLIGHT slots and block the fast direct-DEX fires. A pair with no
        // route fast-fails here (µs); recapture it by ADDING a pool.
        let _ = max_swap_accounts;
        return Err(anyhow!("no direct-DEX route for {}→{} — add a pool", c.asset_mint, c.debt_mint));
    };
    // Jupiter's route ALTs + our liquidation ALT (the fixed marginfi accounts).
    let liq_alt = Pubkey::from_str(
        &std::env::var("LIQ_ALT").unwrap_or_else(|_| LIQ_ALT.into()))?;
    let mut alt_addrs = swap_alts.clone();
    alt_addrs.push(liq_alt);
    let alts = jup::fetch_alts(rpc_endpoint, &alt_addrs)?;

    let asset_ata = ata_for(authority, &c.asset_mint, &c.asset_token_program);
    let debt_ata = ata_for(authority, &c.debt_mint, &c.debt_token_program);

    let mut ixs = vec![
        cu_limit_ix(FIRE_CU_LIMIT),
        cu_price_ix(priority_micro_lamports),
        create_ata_idempotent_for(authority, &c.asset_mint, &c.asset_token_program),
        create_ata_idempotent_for(authority, &c.debt_mint, &c.debt_token_program),
    ];
    if let Some(ix) = mid_ata_ix { ixs.push(ix); }
    let _ = token_program;
    let start_idx = ixs.len();
    ixs.push(marginfi::start_flashloan(liquidator_ma, authority, 0)); // end_index patched below
    ixs.push(marginfi::lending_account_liquidate(
        &c.asset_bank, &c.liab_bank, liquidator_ma, authority, &c.liquidatee,
        &token_program, c.asset_amount, &c.asset_oracle, &c.liab_oracle, &c.liquidatee_obs,
    ));
    ixs.push(marginfi::lending_account_withdraw(
        liquidator_ma, authority, &c.asset_bank, &asset_ata, &c.asset_token_program, c.asset_amount, true,
    ));
    ixs.extend(swap_ixs);
    // repay_all clears the entire liability regardless of amount (verified in
    // marginfi_probe); pass the quoted swap output as a plausible amount. Uses
    // the generic payback for the actual debt bank (USDC/USDT/wSOL).
    ixs.push(marginfi::payback_asset(liquidator_ma, authority, &c.liab_bank, &debt_ata, quoted_out, true));
    // withdraw_all + repay_all close both balances → end_flashloan health check
    // runs over zero active balances (empty observation list).
    let end_index = ixs.len() as u64;
    ixs[start_idx] = marginfi::start_flashloan(liquidator_ma, authority, end_index);
    ixs.push(marginfi::end_flashloan(liquidator_ma, authority, &[]));
    // Tip last, in-tx → only paid when the liquidation lands.
    if let (Some(tip_to), true) = (tip_account, tip_lamports > 0) {
        ixs.push(transfer_ix(*authority, tip_to, tip_lamports));
    }

    let msg = v0::Message::try_compile(authority, &ixs, &alts, blockhash)
        .map_err(|e| anyhow!("compile v0: {e}"))?;
    let tx = VersionedTransaction {
        signatures: vec![solana_signature::Signature::default()],
        message: VersionedMessage::V0(msg),
    };
    let tx_bytes = bincode::serialize(&tx)?.len();
    Ok(FireTx { tx, quoted_usdc_out: quoted_out, tx_bytes })
}

#[cfg(test)]
mod route_tests {
    use super::*;

    const BONK: &str = "DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263";
    const JITOSOL: &str = "J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn";
    const USDS: &str = "USDSwr9ApdHk5bvJKMjzff41FfuX8bSxdKcR81vTwcA";

    #[test]
    fn bonk_usdt_routes_two_hop_via_sol() {
        let (bonk, usdt) = (Pubkey::from_str(BONK).unwrap(), Pubkey::from_str(USDT).unwrap());
        assert!(direct_dex_pool(&bonk, &usdt).is_none(), "no single-hop BONK/USDT pool exists on-chain");
        let (p1, mid, p2) = two_hop_route(&bonk, &usdt).expect("BONK->USDT must route via SOL");
        assert_eq!(mid, Pubkey::from_str(WSOL).unwrap());
        assert_eq!(p1, direct_dex_pool(&bonk, &mid).unwrap());
        assert_eq!(p2, Pubkey::from_str("FwewVm8u6tFPGewAyHmWAqad9hmF7mvqxK4mJ7iNqqGC").unwrap()); // SOL/USDT
        assert!(swap_route_exists(&bonk, &usdt));
    }

    #[test]
    fn exotic_debts_route() {
        let bonk = Pubkey::from_str(BONK).unwrap();
        // whale legs: BONK->USDS (via USDC after SOL misses), BONK->JitoSOL (via SOL)
        assert!(swap_route_exists(&bonk, &Pubkey::from_str(USDS).unwrap()));
        assert!(swap_route_exists(&bonk, &Pubkey::from_str(JITOSOL).unwrap()));
        // direct legs unaffected
        assert!(swap_route_exists(&bonk, &Pubkey::from_str(USDC).unwrap()));
        assert!(direct_dex_pool(&bonk, &Pubkey::from_str(USDC).unwrap()).is_some());
    }

    #[test]
    fn pair_lookup_is_bidirectional() {
        // (USDS, USDC) is stored one way; the reverse must resolve for hop-2 use.
        let (usds, usdc) = (Pubkey::from_str(USDS).unwrap(), Pubkey::from_str(USDC).unwrap());
        assert_eq!(pool_for_pair(&usdc, &usds), pool_for_pair(&usds, &usdc));
        assert!(pool_for_pair(&usdc, &usds).is_some());
    }
}
