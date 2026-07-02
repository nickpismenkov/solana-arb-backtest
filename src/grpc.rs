//! Yellowstone gRPC account-subscription feed → price Ticks. The swappable
//! measurement feed (a ShredStream feed will emit the same Tick later). Sends
//! ticks over an unbounded channel so the harness owns the detector loop.

use anyhow::Result;
use futures::StreamExt;
use std::collections::HashMap;
use std::time::{SystemTime, UNIX_EPOCH};
use tokio::sync::mpsc::UnboundedSender;
use yellowstone_grpc_client::{ClientTlsConfig, GeyserGrpcClient};
use yellowstone_grpc_proto::prelude::{
    subscribe_update::UpdateOneof, CommitmentLevel, SubscribeRequest, SubscribeRequestFilterAccounts,
};

use crate::detector::Tick;
use crate::pools::{orca_price, ray_clmm_price, ORCA_POOL, RAY_CLMM_POOL};

fn now_ms() -> u128 {
    SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_millis()
}

pub async fn run_grpc_feed(
    endpoint: String,
    x_token: String,
    tx: UnboundedSender<Tick>,
) -> Result<()> {
    let mut client = GeyserGrpcClient::build_from_shared(endpoint)?
        .x_token(Some(x_token))?
        .tls_config(ClientTlsConfig::new().with_native_roots())?
        .connect()
        .await?;

    let mut accounts = HashMap::new();
    accounts.insert(
        "pools".to_string(),
        SubscribeRequestFilterAccounts {
            account: vec![ORCA_POOL.to_string(), RAY_CLMM_POOL.to_string()],
            owner: vec![],
            filters: vec![],
            ..Default::default()
        },
    );
    let request = SubscribeRequest {
        accounts,
        commitment: Some(CommitmentLevel::Processed as i32),
        ..Default::default()
    };

    let (mut _sink, mut stream) = client.subscribe_with_request(Some(request)).await?;
    while let Some(message) = stream.next().await {
        let msg = message?;
        if let Some(UpdateOneof::Account(acc)) = msg.update_oneof {
            let slot = acc.slot;
            if let Some(info) = acc.account {
                let pk = bs58::encode(&info.pubkey).into_string();
                let (venue, price) = if pk == ORCA_POOL {
                    ("Orca", orca_price(&info.data))
                } else if pk == RAY_CLMM_POOL {
                    ("Raydium", ray_clmm_price(&info.data))
                } else {
                    continue;
                };
                if let Some(price) = price {
                    let _ = tx.send(Tick {
                        venue,
                        price,
                        slot,
                        ts_ms: now_ms(),
                    });
                }
            }
        }
    }
    Ok(())
}
