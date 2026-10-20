// Drives the twin's NMX-C with NICo's own client crate, libnmxc (crates/libnmxc), the way NICo's
// nvlink-manager does: a client pool with the CA and TLS authority NICo's [nvlink_config] gives it,
// gateway id "carbide", every read checked by libnmxc's own server_header check. Subscribe is
// not in libnmxc's Nmxc trait, so it is called through the NmxControllerClient libnmxc generates
// from the same definition. Nothing here is the twin's code.
//
// Environment: NMXC_URL (https://host:port), NMXC_CA (PEM), NMXC_AUTHORITY (TLS name),
// NMXC_INJECT (a shell command that injects a fault on the twin once the subscription is up),
// NMXC_EXPECT_GPUS (default 72).
use std::path::PathBuf;
use std::process::ExitCode;
use std::time::Duration;

use libnmxc::nmxc_model as nmx;
use libnmxc::nmxc_model::nmx_controller_client::NmxControllerClient;
use libnmxc::{Endpoint, NMX_C_GATEWAY_ID, Nmxc, NmxcClientPool, NmxcTlsConfig};

type Res<T> = Result<T, Box<dyn std::error::Error>>;

fn env(k: &str) -> String {
    std::env::var(k).unwrap_or_else(|_| panic!("{k} is required"))
}

fn ctx() -> Option<nmx::Context> {
    Some(nmx::Context { context: String::new() })
}

fn health(v: i32) -> &'static str {
    nmx::GpuHealth::try_from(v).map(|h| h.as_str_name()).unwrap_or("?")
}

async fn gpus(c: &mut Box<dyn Nmxc>) -> Res<Vec<nmx::GpuInfo>> {
    let r = c
        .get_gpu_info_list(nmx::GetGpuInfoListRequest {
            context: ctx(),
            attr: nmx::GpuAttr::NmxGpuAttrAll as i32,
            gateway_id: NMX_C_GATEWAY_ID.into(),
            ..Default::default()
        })
        .await?;
    Ok(r.gpu_info_list)
}

async fn partitions(c: &mut Box<dyn Nmxc>) -> Res<Vec<nmx::PartitionInfo>> {
    let r = c
        .get_partition_info_list(nmx::GetPartitionInfoListRequest {
            context: ctx(),
            gateway_id: NMX_C_GATEWAY_ID.into(),
            ..Default::default()
        })
        .await?;
    Ok(r.partition_info_list)
}

fn pid(id: u32) -> Option<nmx::PartitionId> {
    Some(nmx::PartitionId { partition_id: id })
}

fn check(ok: bool, what: &str) -> Res<()> {
    if ok {
        println!("  ok: {what}");
        Ok(())
    } else {
        Err(format!("FAILED: {what}").into())
    }
}

async fn run() -> Res<()> {
    let url = env("NMXC_URL");
    let ca = PathBuf::from(env("NMXC_CA"));
    let authority = env("NMXC_AUTHORITY");
    let want: usize = std::env::var("NMXC_EXPECT_GPUS").ok().and_then(|v| v.parse().ok()).unwrap_or(72);

    let pool = NmxcClientPool::builder()
        .timeout(Duration::from_secs(10))
        .tls(NmxcTlsConfig {
            ca_cert_path: Some(ca.clone()),
            client_cert_path: None,
            client_key_path: None,
            authority: Some(authority.clone()),
        })
        .build()?;
    let mut c = pool.create_client(Endpoint::new(&url)?).await?;
    println!("libnmxc client connected to {url} (authority {authority})");

    let hello = c.hello(NMX_C_GATEWAY_ID).await?;
    let h = hello.server_header.clone().unwrap_or_default();
    println!(
        "Hello: domain_uuid={} app_ver={} components={:?} capabilities={:?} version {}.{}",
        h.domain_uuid,
        h.app_ver,
        hello.components_ver.iter().map(|kv| format!("{}={}", kv.key, kv.value)).collect::<Vec<_>>(),
        hello.capabilities,
        hello.major_version,
        hello.minor_version
    );
    check(!h.domain_uuid.is_empty(), "Hello carries the domain UUID NICo records for the rack")?;

    let p = c.get_domain_properties(None, NMX_C_GATEWAY_ID).await?;
    println!(
        "GetDomainProperties: {} compute nodes x {} GPUs ({} NVLinks each), {} switch nodes x {} switches, {} partitions max",
        p.max_compute_nodes, p.max_gpus_per_compute_node, p.max_gpu_nv_links, p.max_switch_nodes, p.max_switches_per_switch_node, p.max_num_partitions
    );

    let s = c.get_domain_state_info(None, NMX_C_GATEWAY_ID).await?;
    let cps = nmx::ControlPlaneState::try_from(s.control_plane_state).map(|v| v.as_str_name()).unwrap_or("?");
    println!(
        "GetDomainStateInfo: {cps} ({}) controller {}",
        s.config_status_description,
        nmx::NmxControllerHealth::try_from(s.nmx_controller_health).map(|v| v.as_str_name()).unwrap_or("?")
    );
    check(cps == "NMX_CONTROL_PLANE_STATE_CONFIGURED", "the domain's control plane is configured")?;

    let list = gpus(&mut c).await?;
    println!("GetGpuInfoList: {} GPUs", list.len());
    for g in list.iter().take(3) {
        let loc = g.loc.clone().unwrap_or_default();
        println!(
            "  gpu_uid={:#018x} gpu_id={} health={} chassis={} tray_index={} slot={} partition={:?}",
            g.gpu_uid,
            g.gpu_id,
            health(g.gpu_health),
            loc.chassis_serial_number,
            loc.tray_index,
            loc.location.map(|l| l.slot_id).unwrap_or_default(),
            g.partition_id.as_ref().map(|p| p.partition_id)
        );
    }
    check(list.len() == want, &format!("{want} GPUs in the NVLink domain"))?;
    check(list.iter().all(|g| health(g.gpu_health) == "NMX_GPU_HEALTH_HEALTHY"), "every GPU healthy")?;

    // NICo's partition lifecycle on a fresh controller: the factory partition goes first
    let before = partitions(&mut c).await?;
    println!(
        "GetPartitionInfoList: {:?}",
        before.iter().map(|p| (p.partition_id.as_ref().map(|i| i.partition_id), p.name.clone(), p.num_gpus)).collect::<Vec<_>>()
    );
    for p in &before {
        let id = p.partition_id.as_ref().map(|i| i.partition_id).unwrap_or_default();
        c.delete_partition(nmx::DeletePartitionRequest { context: ctx(), partition_id: pid(id), gateway_id: NMX_C_GATEWAY_ID.into(), name: String::new() })
            .await?;
        println!("DeletePartition {id}: ok");
    }
    let uids: Vec<u64> = list.iter().take(8).map(|g| g.gpu_uid).collect();
    let created = c
        .create_partition(nmx::CreatePartitionRequest {
            context: ctx(),
            name: "dctwin-tenant-a".into(),
            gpu_resource_id: uids
                .iter()
                .map(|u| nmx::GpuResourceId { resource_id: Some(nmx::gpu_resource_id::ResourceId::GpuUid(*u)) })
                .collect(),
            attr: None,
            partition_id: None,
            gateway_id: NMX_C_GATEWAY_ID.into(),
        })
        .await?;
    let new_id = created.partition_id.map(|p| p.partition_id).unwrap_or_default();
    println!("CreatePartition dctwin-tenant-a with 8 GPUs: partition_id={new_id}");
    let in_use = c
        .create_partition(nmx::CreatePartitionRequest {
            context: ctx(),
            name: "dctwin-tenant-b".into(),
            gpu_resource_id: vec![nmx::GpuResourceId { resource_id: Some(nmx::gpu_resource_id::ResourceId::GpuUid(uids[0])) }],
            gateway_id: NMX_C_GATEWAY_ID.into(),
            ..Default::default()
        })
        .await;
    let code = in_use.as_ref().err().and_then(|e| e.nmx_return_code());
    println!("CreatePartition with a GPU already in a partition: {:?}", in_use.as_ref().err().map(|e| e.to_string()));
    check(code == Some(nmx::StReturnCode::NmxStResourceInUse as i32), "a GPU in two partitions is NMX_ST_RESOURCE_IN_USE")?;
    c.add_gpus_to_partition(nmx::UpdatePartitionRequest {
        context: ctx(),
        partition_id: pid(new_id),
        gpu_uid: vec![list[8].gpu_uid],
        gateway_id: NMX_C_GATEWAY_ID.into(),
        ..Default::default()
    })
    .await?;
    c.remove_gpus_from_partition(nmx::UpdatePartitionRequest {
        context: ctx(),
        partition_id: pid(new_id),
        gpu_uid: vec![uids[0]],
        gateway_id: NMX_C_GATEWAY_ID.into(),
        ..Default::default()
    })
    .await?;
    let now = partitions(&mut c).await?;
    let mine = now.iter().find(|p| p.name == "dctwin-tenant-a").ok_or("partition not listed")?;
    println!(
        "GetPartitionInfoList: dctwin-tenant-a id={new_id} num_gpus={} health={} type={}",
        mine.num_gpus,
        nmx::PartitionHealth::try_from(mine.health).map(|v| v.as_str_name()).unwrap_or("?"),
        nmx::PartitionType::try_from(mine.partition_type).map(|v| v.as_str_name()).unwrap_or("?")
    );
    check(mine.num_gpus == 8 && mine.gpu_uid_list.contains(&list[8].gpu_uid) && !mine.gpu_uid_list.contains(&uids[0]), "add and remove GPUs")?;
    let count = c
        .get_partition_count(nmx::GetPartitionCountRequest {
            context: ctx(),
            info_attr: nmx::PartitionInfoAttr::NmxPartitionInfoAttrAll as i32,
            gateway_id: NMX_C_GATEWAY_ID.into(),
            ..Default::default()
        })
        .await?;
    check(count.num_partitions == 1, "GetPartitionCount 1")?;
    c.delete_partition(nmx::DeletePartitionRequest { context: ctx(), partition_id: None, gateway_id: NMX_C_GATEWAY_ID.into(), name: "dctwin-tenant-a".into() })
        .await?;
    check(partitions(&mut c).await?.is_empty(), "DeletePartition by name leaves none")?;

    // Subscribe (Q8), through libnmxc's generated client on a channel with the same TLS
    let tls = tonic::transport::ClientTlsConfig::new()
        .ca_certificate(tonic::transport::Certificate::from_pem(std::fs::read(&ca)?))
        .domain_name(authority.clone());
    let channel = tonic::transport::Endpoint::from_shared(url.clone())?.tls_config(tls)?.connect().await?;
    let mut raw = NmxControllerClient::new(channel);
    let mut stream = raw
        .subscribe(nmx::SubscribeRequest { gateway_id: "dctwin-subscriber".into(), notify_on_self_change: false, heart_beat_rate: 0 })
        .await?
        .into_inner();
    let first = stream.message().await?.ok_or("the stream ended")?;
    println!("Subscribe: first notification {}", kind(&first));
    check(matches!(first.notification, Some(nmx::server_notification::Notification::SubscriptionResponse(_))), "SubscriptionResponse first")?;
    let inject = env("NMXC_INJECT");
    println!("injecting on the twin: {inject}");
    let st = std::process::Command::new("sh").arg("-c").arg(&inject).status()?;
    check(st.success(), "fault injected")?;
    let (mut health_seen, mut topo_seen) = (false, false);
    while !(health_seen && topo_seen) {
        let n = tokio::time::timeout(Duration::from_secs(20), stream.message()).await??.ok_or("the stream ended")?;
        let k = kind(&n);
        println!("Subscribe: notification {k}");
        health_seen |= k == "HealthStateChanged";
        topo_seen |= k.starts_with("FmEvent");
    }
    let s = c.get_domain_state_info(None, NMX_C_GATEWAY_ID).await?;
    let after = gpus(&mut c).await?;
    let mut by: std::collections::BTreeMap<&str, usize> = Default::default();
    for g in &after {
        *by.entry(health(g.gpu_health)).or_default() += 1;
    }
    println!(
        "after the fault: GetDomainStateInfo {} ({}); GetGpuInfoList health {:?}",
        nmx::ControlPlaneState::try_from(s.control_plane_state).map(|v| v.as_str_name()).unwrap_or("?"),
        s.config_status_description,
        by
    );
    check(!by.contains_key("NMX_GPU_HEALTH_HEALTHY") || by.len() > 1, "GPU health follows the fault")?;
    Ok(())
}

fn kind(n: &nmx::ServerNotification) -> String {
    use nmx::server_notification::Notification as N;
    match &n.notification {
        Some(N::SubscriptionResponse(_)) => "SubscriptionResponse".into(),
        Some(N::StaticConfigResponse(_)) => "StaticConfigResponse".into(),
        Some(N::CreatePartitionResponse(_)) => "CreatePartitionResponse".into(),
        Some(N::DeletePartitionResponse(_)) => "DeletePartitionResponse".into(),
        Some(N::UpdatePartitionResponse(_)) => "UpdatePartitionResponse".into(),
        Some(N::FmEvent(e)) => match &e.event {
            Some(nmx::fm_event::Event::FmEventControlPlaneStateChange(_)) => "FmEvent/ControlPlaneStateChange".into(),
            Some(nmx::fm_event::Event::FmEventTopologyChange(_)) => "FmEvent/TopologyChange".into(),
            Some(nmx::fm_event::Event::FmEventPartitionChange(p)) => format!("FmEvent/PartitionChange {:?}", p.partition_id),
            None => "FmEvent".into(),
        },
        Some(N::HealthStateChanged(_)) => "HealthStateChanged".into(),
        Some(N::SetAdminStateResponse(_)) => "SetAdminStateResponse".into(),
        Some(N::DomainStateInfo(_)) => "DomainStateInfo".into(),
        Some(N::InitDone(_)) => "InitDone".into(),
        None => "empty".into(),
    }
}

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => {
            println!("nmxc_check: PASS");
            ExitCode::SUCCESS
        }
        Err(e) => {
            println!("nmxc_check: {e}");
            ExitCode::FAILURE
        }
    }
}
