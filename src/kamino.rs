//! Kamino Lend (klend) liquidation finder — Stage 1, read-only.
//!
//! Unlike marginfi (where we decode Pyth oracles and compute health), Kamino's
//! `Obligation` STORES pre-computed USD health values as `Fraction` fixed-point
//! (u128, 60 fractional bits). So we read liquidatability straight from the
//! obligation — no oracle needed:
//!
//!     liquidatable  ⟺  borrow_factor_adjusted_debt_value ≥ unhealthy_borrow_value
//!
//! Caveat: these values are as of the obligation's last on-chain refresh; the
//! `stale` flag + last_update.slot tell us how fresh they are. Good enough for a
//! finder; a live trigger would re-price via Scope (Kamino's oracle, not Pyth).
//!
//! All offsets VERIFIED against a real 3344-byte main-market obligation: the
//! stored allowed/unhealthy values equal deposited_value × the init/liq LTVs
//! (0.80 / 0.90), which only holds if every offset is correct.

use solana_pubkey::Pubkey;

pub const KLEND_PROGRAM: &str = "KLend2g3cP87fffoy8q1mQqGKjrxjC8boSyAYavgmjD";
pub const KAMINO_MAIN_MARKET: &str = "7u3HeHxYDLhnCoErrtycNokbQYbWGzLs6JSDqGAv5PfF";

/// account:Obligation Anchor discriminator, and total account size.
pub const OBLIGATION_DISC: [u8; 8] = [168, 206, 141, 106, 88, 76, 172, 167];
pub const OBLIGATION_SIZE: usize = 3344;

// Kamino `Fraction` = u128 with 60 fractional bits (value / 2^60).
const FRACTION_BITS: u32 = 60;

// VERIFIED offsets (offset 0 = 8-byte discriminator).
const O_LAST_UPDATE_SLOT: usize = 16;
const O_STALE: usize = 24;
const O_LENDING_MARKET: usize = 32;
const O_OWNER: usize = 64;
const O_DEPOSITED_VALUE: usize = 1192;
const O_BF_ADJ_DEBT_VALUE: usize = 2208;
const O_BORROWED_VALUE: usize = 2224;
const O_ALLOWED_BORROW_VALUE: usize = 2240;
const O_UNHEALTHY_BORROW_VALUE: usize = 2256;

fn frac_at(data: &[u8], off: usize) -> f64 {
    let mut b = [0u8; 16];
    b.copy_from_slice(&data[off..off + 16]);
    u128::from_le_bytes(b) as f64 / (2f64).powi(FRACTION_BITS as i32)
}

fn pubkey_at(data: &[u8], off: usize) -> Pubkey {
    let mut b = [0u8; 32];
    b.copy_from_slice(&data[off..off + 32]);
    Pubkey::new_from_array(b)
}

/// A decoded Kamino borrower position with its stored USD health values.
#[derive(Clone, Debug)]
pub struct Obligation {
    pub owner: Pubkey,
    pub lending_market: Pubkey,
    pub last_update_slot: u64,
    pub stale: bool,
    /// Total deposited collateral value (USD) — what's seizable.
    pub deposited_value: f64,
    /// Borrow-factor-adjusted debt — the value compared against the threshold.
    pub bf_adjusted_debt: f64,
    /// Raw borrowed market value (USD).
    pub borrowed_value: f64,
    /// Borrow allowed at init LTV (USD).
    pub allowed_borrow_value: f64,
    /// Liquidation threshold (USD) — cross it and you're liquidatable.
    pub unhealthy_borrow_value: f64,
}

impl Obligation {
    pub fn decode(data: &[u8]) -> Option<Obligation> {
        if data.len() < O_UNHEALTHY_BORROW_VALUE + 16 || data[..8] != OBLIGATION_DISC {
            return None;
        }
        Some(Obligation {
            owner: pubkey_at(data, O_OWNER),
            lending_market: pubkey_at(data, O_LENDING_MARKET),
            last_update_slot: u64::from_le_bytes(data[O_LAST_UPDATE_SLOT..O_LAST_UPDATE_SLOT + 8].try_into().ok()?),
            stale: data[O_STALE] != 0,
            deposited_value: frac_at(data, O_DEPOSITED_VALUE),
            bf_adjusted_debt: frac_at(data, O_BF_ADJ_DEBT_VALUE),
            borrowed_value: frac_at(data, O_BORROWED_VALUE),
            allowed_borrow_value: frac_at(data, O_ALLOWED_BORROW_VALUE),
            unhealthy_borrow_value: frac_at(data, O_UNHEALTHY_BORROW_VALUE),
        })
    }

    /// Kamino's own liquidation gate.
    pub fn liquidatable(&self) -> bool {
        self.bf_adjusted_debt >= self.unhealthy_borrow_value && self.unhealthy_borrow_value > 0.0
    }

    /// debt / threshold — ≥ 1.0 means liquidatable; how close otherwise.
    pub fn ratio(&self) -> f64 {
        if self.unhealthy_borrow_value == 0.0 {
            0.0
        } else {
            self.bf_adjusted_debt / self.unhealthy_borrow_value
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn healthy_when_debt_below_threshold() {
        let o = Obligation {
            owner: Pubkey::default(), lending_market: Pubkey::default(),
            last_update_slot: 0, stale: false,
            deposited_value: 2513.33, bf_adjusted_debt: 900.49, borrowed_value: 720.39,
            allowed_borrow_value: 2010.66, unhealthy_borrow_value: 2262.0,
        };
        assert!(!o.liquidatable());
        assert!(o.ratio() < 1.0);
    }

    #[test]
    fn liquidatable_when_debt_crosses() {
        let o = Obligation {
            owner: Pubkey::default(), lending_market: Pubkey::default(),
            last_update_slot: 0, stale: false,
            deposited_value: 1000.0, bf_adjusted_debt: 910.0, borrowed_value: 900.0,
            allowed_borrow_value: 800.0, unhealthy_borrow_value: 900.0,
        };
        assert!(o.liquidatable());
        assert!(o.ratio() >= 1.0);
    }
}
