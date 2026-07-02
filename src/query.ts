// GraphQL builders for Bitquery Solana DEXTradeByTokens (the aggregation cube).
// We fetch per-(minute, venue) OHLC of BASE priced in QUOTE (USDC per SOL).

import { BASE, QUOTE } from './config.js';

export interface OhlcRow {
  Block: { Time: string };
  Trade: {
    Dex: { ProtocolFamily: string };
    open: number;
    high: number;
    low: number;
    close: number;
  };
  count: string;
}
export interface OhlcResponse {
  Solana: { DEXTradeByTokens: OhlcRow[] };
}

export function ohlcQuery(opts: {
  venues: string[];
  after: string;
  before: string;
  intervalMinutes: number;
  minUsd: number;
}): string {
  const venueList = opts.venues.map((v) => `"${v}"`).join(', ');
  return `
    query {
      Solana {
        DEXTradeByTokens(
          where: {
            Trade: {
              Currency: { MintAddress: { is: "${BASE.mint}" } },
              Side: { Currency: { MintAddress: { is: "${QUOTE.mint}" } }, AmountInUSD: { gt: "${opts.minUsd}" } },
              Dex: { ProtocolFamily: { in: [${venueList}] } }
            },
            Block: { Time: { after: "${opts.after}", before: "${opts.before}" } }
          }
          orderBy: { ascendingByField: "Block_Time" }
          limit: { count: 100000 }
        ) {
          Block { Time(interval: { in: minutes, count: ${opts.intervalMinutes} }) }
          Trade {
            Dex { ProtocolFamily }
            open:  Price(minimum: Block_Slot)
            close: Price(maximum: Block_Slot)
            high:  Price(maximum: Trade_Price)
            low:   Price(minimum: Trade_Price)
          }
          count
        }
      }
    }
  `;
}
