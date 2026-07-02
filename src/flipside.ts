// Thin Flipside Compass wrapper. The SDK submits SQL, polls the run to
// completion, and returns rows — so we just hand it a query string.

import { Flipside } from '@flipsidecrypto/sdk';

export type Row = Record<string, unknown>;

export async function runSql(sql: string): Promise<Row[]> {
  const key = process.env.FLIPSIDE_API_KEY;
  if (!key) {
    throw new Error(
      'FLIPSIDE_API_KEY is not set. Get a free key at https://flipsidecrypto.xyz ' +
        '(Settings → API Keys), copy .env.example to .env, and fill it in.',
    );
  }
  const flipside = new Flipside(key, 'https://api-v2.flipsidecrypto.xyz');
  const result = await flipside.query.run({
    sql,
    maxAgeMinutes: 60, // reuse a cached run if we re-run within the hour
    pageSize: 100_000,
    timeoutMinutes: 20,
  });
  // Normalize keys to lowercase so downstream code is stable regardless of how
  // the column casing comes back.
  return (result.records ?? []).map((r) => {
    const o: Row = {};
    for (const [k, v] of Object.entries(r as Row)) o[k.toLowerCase()] = v;
    return o;
  });
}
