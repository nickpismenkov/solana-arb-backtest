//! Jupiter Lend (Fluid) liquidation data layer — vault decoders + first-pass
//! liquidatable detection.
//!
//! Jupiter Lend is a FLUID vault-tick protocol, fundamentally different from the
//! obligation-based lenders (marginfi/Kamino/Save): liquidation is VAULT-LEVEL,
//! not per-borrower. Each vault is an isolated (supply_token = collateral,
//! borrow_token = debt) pair; a liquidator buys collateral off the vault's
//! liquidation curve at a penalty discount (see the `liquidate` ix:
//! debt_amt + col_per_unit_debt + absorb). There is NO obligation account.
//!
//! Layouts are Anchor/borsh (8-byte account discriminator = sha256("account:
//! <Name>")[..8], then fields in declaration order, packed, little-endian),
//! taken from the published IDL (jup-ag/jupiter-lend target/idl/vaults.json) and
//! VERIFIED against 89 live mainnet VaultConfig/VaultState accounts.
//!
//! Program (Vaults/Borrow): jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi

use solana_pubkey::Pubkey;

pub const VAULTS_PROGRAM: &str = "jupr81YtYssSyPt8jbnGuiWon5f6x9TcDEFxYe3Bdzi";
pub const LIQUIDITY_PROGRAM: &str = "jupeiUmn818Jg1ekPURTpr4mFo29p46vygyykFJ3wZC";

// Anchor account discriminators (sha256("account:<Name>")[..8]).
pub const VAULT_CONFIG_DISC: [u8; 8] = [0x63, 0x56, 0x2b, 0xd8, 0xb8, 0x66, 0x77, 0x4d];
pub const VAULT_STATE_DISC: [u8; 8] = [0xe4, 0xc4, 0x52, 0xa5, 0x62, 0xd2, 0xeb, 0x98];

// Common debt-token mints (the reachable debt set for our liquidator).
pub const USDC_MINT: &str = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v";
pub const USDT_MINT: &str = "Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB";
pub const WSOL_MINT: &str = "So11111111111111111111111111111111111111112";

fn read_pk(d: &[u8], o: usize) -> Option<Pubkey> {
    Some(Pubkey::new_from_array(d.get(o..o + 32)?.try_into().ok()?))
}
fn u16le(d: &[u8], o: usize) -> Option<u16> { Some(u16::from_le_bytes(d.get(o..o + 2)?.try_into().ok()?)) }
fn u32le(d: &[u8], o: usize) -> Option<u32> { Some(u32::from_le_bytes(d.get(o..o + 4)?.try_into().ok()?)) }
fn u64le(d: &[u8], o: usize) -> Option<u64> { Some(u64::from_le_bytes(d.get(o..o + 8)?.try_into().ok()?)) }
fn u128le(d: &[u8], o: usize) -> Option<u128> { Some(u128::from_le_bytes(d.get(o..o + 16)?.try_into().ok()?)) }
fn i32le(d: &[u8], o: usize) -> Option<i32> { Some(i32::from_le_bytes(d.get(o..o + 4)?.try_into().ok()?)) }

// ── VaultConfig (219 bytes; VERIFIED against live accounts) ──────────────────
// disc[8] · vault_id u16@8 · supply_rate_magnifier i16@10 · borrow_rate_magnifier
// i16@12 · collateral_factor u16@14 · liquidation_threshold u16@16 ·
// liquidation_max_limit u16@18 · withdraw_gap u16@20 · liquidation_penalty u16@22
// · borrow_fee u8@24 · vault_type u8@25 · oracle @26 · rebalancer @58 ·
// liquidity_program @90 · oracle_program @122 · supply_token @154 ·
// borrow_token @186 · bump u8@218.
#[derive(Clone, Debug)]
pub struct VaultConfig {
    pub vault_id: u16,
    /// Raw u16. INFERRED scale: units of 0.1% (raw/1000 = fraction, raw/10 =
    /// percent) — matches known LTVs (wSOL/USDC 850→85%, JUP/USDC 700→70%), but
    /// treat as inferred; the fire path's sim is ground truth regardless.
    pub collateral_factor: u16,
    pub liquidation_threshold: u16,
    pub liquidation_penalty: u16,
    pub oracle: Pubkey,
    pub oracle_program: Pubkey,
    /// Collateral mint.
    pub supply_token: Pubkey,
    /// Debt mint (what the liquidator repays).
    pub borrow_token: Pubkey,
}

impl VaultConfig {
    pub fn decode(d: &[u8]) -> Option<VaultConfig> {
        if d.len() < 219 || d[..8] != VAULT_CONFIG_DISC { return None; }
        Some(VaultConfig {
            vault_id: u16le(d, 8)?,
            collateral_factor: u16le(d, 14)?,
            liquidation_threshold: u16le(d, 16)?,
            liquidation_penalty: u16le(d, 22)?,
            oracle: read_pk(d, 26)?,
            oracle_program: read_pk(d, 122)?,
            supply_token: read_pk(d, 154)?,
            borrow_token: read_pk(d, 186)?,
        })
    }
    /// Liquidation threshold as a fraction (INFERRED scale, see field docs).
    pub fn liq_threshold_frac(&self) -> f64 { self.liquidation_threshold as f64 / 1000.0 }
    /// Debt-token label for the reachable-set check.
    pub fn debt_label(&self) -> &'static str {
        match self.borrow_token.to_string().as_str() {
            USDC_MINT => "USDC", USDT_MINT => "USDT", WSOL_MINT => "wSOL", _ => "other",
        }
    }
    /// Debt we currently target (USDC/USDT/SOL).
    pub fn debt_in_scope(&self) -> bool { self.debt_label() != "other" }
}

// ── VaultState (127 bytes; VERIFIED against live accounts) ───────────────────
// disc[8] · vault_id u16@8 · branch_liquidated u8@10 · topmost_tick i32@11 ·
// current_branch_id u32@15 · total_branch_id u32@19 · total_supply u64@23 ·
// total_borrow u64@31 · total_positions u32@39 · absorbed_debt_amount u128@43 ·
// absorbed_col_amount u128@59 · absorbed_dust_debt u64@75 · liquidity_supply_
// exchange_price u64@83 · liquidity_borrow_exchange_price u64@91 · vault_supply_
// exchange_price u64@99 · vault_borrow_exchange_price u64@107 · next_position_id
// u32@115 · last_update_timestamp u64@119.
#[derive(Clone, Debug)]
pub struct VaultState {
    pub vault_id: u16,
    /// Nonzero while a branch is mid-liquidation.
    pub branch_liquidated: u8,
    /// Highest debt tick (Fluid liquidation curve position).
    pub topmost_tick: i32,
    pub total_supply: u64,
    pub total_borrow: u64,
    pub total_positions: u32,
    /// Debt the vault has absorbed (bad-debt / pending-liquidation bucket).
    pub absorbed_debt_amount: u128,
    pub vault_supply_exchange_price: u64,
    pub vault_borrow_exchange_price: u64,
    pub last_update_timestamp: u64,
}

impl VaultState {
    pub fn decode(d: &[u8]) -> Option<VaultState> {
        if d.len() < 127 || d[..8] != VAULT_STATE_DISC { return None; }
        Some(VaultState {
            vault_id: u16le(d, 8)?,
            branch_liquidated: *d.get(10)?,
            topmost_tick: i32le(d, 11)?,
            total_supply: u64le(d, 23)?,
            total_borrow: u64le(d, 31)?,
            total_positions: u32le(d, 39)?,
            absorbed_debt_amount: u128le(d, 43)?,
            vault_supply_exchange_price: u64le(d, 99)?,
            vault_borrow_exchange_price: u64le(d, 107)?,
            last_update_timestamp: u64le(d, 119)?,
        })
    }
}

/// A vault = its config + state, keyed by vault_id.
#[derive(Clone, Debug)]
pub struct Vault {
    pub config_pubkey: Pubkey,
    pub state_pubkey: Pubkey,
    pub config: VaultConfig,
    pub state: VaultState,
}

impl Vault {
    /// FIRST-PASS liquidatable signal. HONEST STATUS:
    ///  - CONFIDENT: `absorbed_debt_amount > 0` means the vault currently holds
    ///    absorbed/pending-liquidation debt — a real, unambiguous signal that
    ///    something is (or just was) being liquidated in this vault.
    ///  - NOT USED: `branch_liquidated` is decoded but appears to be a historical
    ///    branch counter (nonzero on ~1/3 of vaults with zero absorbed debt), so
    ///    it is NOT a "liquidatable now" signal and is excluded from this check.
    ///  - INFERRED / NOT YET IMPLEMENTED: precise "is there liquidatable debt at
    ///    the current price" needs Fluid's tick↔price math — comparing
    ///    `topmost_tick` to the tick implied by the live oracle price and
    ///    `liquidation_threshold`. That formula is NOT reversed here. This method
    ///    only surfaces vaults with absorbed debt; the executor's on-chain
    ///    liquidate simulation is the ground-truth gate (as with the other
    ///    protocols). Do NOT treat `true` as "definitely liquidatable," nor
    ///    `false` as "definitely healthy."
    pub fn maybe_liquidatable(&self) -> bool {
        self.state.total_borrow > 0 && self.state.absorbed_debt_amount > 0
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::str::FromStr;

    // Discriminators = sha256("account:VaultConfig"/"VaultState")[..8], computed
    // and cross-checked against the IDL + verified against 89 live accounts
    // during recon (see PR body); no sha2 dep in this crate to assert inline.

    #[test]
    fn debt_label_maps_known_mints() {
        let mk = |borrow: &str| VaultConfig {
            vault_id: 1, collateral_factor: 800, liquidation_threshold: 850, liquidation_penalty: 10,
            oracle: Pubkey::default(), oracle_program: Pubkey::default(),
            supply_token: Pubkey::from_str(WSOL_MINT).unwrap(),
            borrow_token: Pubkey::from_str(borrow).unwrap(),
        };
        assert_eq!(mk(USDC_MINT).debt_label(), "USDC");
        assert_eq!(mk(USDT_MINT).debt_label(), "USDT");
        assert_eq!(mk(WSOL_MINT).debt_label(), "wSOL");
        assert!(mk(USDC_MINT).debt_in_scope());
        assert!((mk(USDC_MINT).liq_threshold_frac() - 0.85).abs() < 1e-9);
    }
}
