// Minimal Bitquery V2 (Solana EAP) GraphQL client. Uses the global fetch (Node 18+).

const ENDPOINT = 'https://streaming.bitquery.io/eap';

export async function gql<T = unknown>(query: string): Promise<T> {
  const token = process.env.BITQUERY_TOKEN;
  if (!token) {
    throw new Error(
      'BITQUERY_TOKEN is not set. Put your Bitquery access token in .env ' +
        '(BITQUERY_TOKEN=...). Get one free at https://ide.bitquery.io.',
    );
  }
  const res = await fetch(ENDPOINT, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: `Bearer ${token}` },
    body: JSON.stringify({ query }),
  });
  if (!res.ok) throw new Error(`Bitquery HTTP ${res.status}: ${await res.text()}`);
  const json = (await res.json()) as { data?: T; errors?: unknown };
  if (json.errors) throw new Error(`Bitquery GraphQL error: ${JSON.stringify(json.errors)}`);
  if (!json.data) throw new Error('Bitquery returned no data.');
  return json.data;
}
