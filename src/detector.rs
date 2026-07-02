//! Feed-agnostic fee-adjusted arb detector — a direct port of the TS version.
//! Feed it price ticks from any source (gRPC now, ShredStream later) and it
//! emits arb open/close with lifetime (slots + ms) = the reaction budget. A
//! "real" arb requires the cross-venue gap to exceed the SUM of both fees.

#[derive(Clone, Debug)]
pub struct Tick {
    pub venue: &'static str,
    pub price: f64, // quote per base (USDC per SOL)
    pub slot: u64,
    pub ts_ms: u128,
}

#[derive(Clone, Debug)]
#[allow(dead_code)] // buy/sell venue + open/close slot consumed by the PR4 report
pub struct ArbEvent {
    pub buy_venue: &'static str,
    pub sell_venue: &'static str,
    pub open_slot: u64,
    pub close_slot: u64,
    pub lifetime_slots: u64,
    pub lifetime_ms: u128,
    pub peak_net_bps: f64,
}

pub enum TickResult {
    None,
    Open { net_bps: f64 },
    Close(ArbEvent),
}

struct OpenState {
    buy: &'static str,
    sell: &'static str,
    open_slot: u64,
    open_ts_ms: u128,
    peak_net_bps: f64,
    last_slot: u64,
    last_ts_ms: u128,
}

pub struct Detector {
    venues: (&'static str, &'static str),
    pub threshold_bps: f64,
    last_a: Option<f64>,
    last_b: Option<f64>,
    open: Option<OpenState>,
}

impl Detector {
    pub fn new(a: &'static str, b: &'static str, fee_a: f64, fee_b: f64) -> Self {
        Self {
            venues: (a, b),
            threshold_bps: fee_a + fee_b,
            last_a: None,
            last_b: None,
            open: None,
        }
    }

    pub fn on_tick(&mut self, t: &Tick) -> TickResult {
        let (a, b) = self.venues;
        if t.venue == a {
            self.last_a = Some(t.price);
        } else if t.venue == b {
            self.last_b = Some(t.price);
        }
        let (pa, pb) = match (self.last_a, self.last_b) {
            (Some(pa), Some(pb)) => (pa, pb),
            _ => return TickResult::None,
        };

        let signed_gap_bps = ((pb - pa) / pa) * 10_000.0; // + => B dearer
        let (net, buy, sell) = if signed_gap_bps > self.threshold_bps {
            (signed_gap_bps - self.threshold_bps, a, b)
        } else if signed_gap_bps < -self.threshold_bps {
            (-signed_gap_bps - self.threshold_bps, b, a)
        } else {
            (0.0, "", "")
        };

        if net > 0.0 {
            match &mut self.open {
                None => {
                    self.open = Some(OpenState {
                        buy,
                        sell,
                        open_slot: t.slot,
                        open_ts_ms: t.ts_ms,
                        peak_net_bps: net,
                        last_slot: t.slot,
                        last_ts_ms: t.ts_ms,
                    });
                    return TickResult::Open { net_bps: net };
                }
                Some(o) => {
                    o.peak_net_bps = o.peak_net_bps.max(net);
                    o.last_slot = t.slot;
                    o.last_ts_ms = t.ts_ms;
                    return TickResult::None;
                }
            }
        }

        if let Some(o) = self.open.take() {
            let close_slot = if t.slot != 0 { t.slot } else { o.last_slot };
            let _ = o.last_ts_ms;
            return TickResult::Close(ArbEvent {
                buy_venue: o.buy,
                sell_venue: o.sell,
                open_slot: o.open_slot,
                close_slot,
                lifetime_slots: close_slot.saturating_sub(o.open_slot),
                lifetime_ms: t.ts_ms.saturating_sub(o.open_ts_ms),
                peak_net_bps: o.peak_net_bps,
            });
        }
        TickResult::None
    }
}

pub fn median_f64(mut v: Vec<f64>) -> f64 {
    if v.is_empty() {
        return 0.0;
    }
    v.sort_by(|x, y| x.partial_cmp(y).unwrap());
    v[v.len() / 2]
}

pub fn median_u128(mut v: Vec<u128>) -> u128 {
    if v.is_empty() {
        return 0;
    }
    v.sort_unstable();
    v[v.len() / 2]
}
