// Drives the twin's Rack Management Service with NVIDIA's own client crate, librms
// (dsx-ai-factory/nv-rms-client at 966d7a7): its TLS client with the estate CA as the root,
// its GetVersion readiness probe and retry policy, and the RPCs NICo's component manager makes,
// with nodes named as NICo names them before ingestion (BMC MAC and address, with credentials).
// Nothing here is the twin's code.
//
// Environment: RMS_URL (https://host:port), RMS_CA (PEM), RMS_INVENTORY (a JSON list of
// {path, rack, class, bmcMAC, siteIP, user, password} for one NVL rack), RMS_TRAY (the path of the
// compute tray to power-cycle and update).
use std::process::ExitCode;
use std::time::Duration;

use librms::client::{RetryConfig, RmsApiConfig};
use librms::client_config::RmsClientConfig;
use librms::protos::rack_manager as rms;
use librms::protos::rack_manager_v2 as rms_v2;
use librms::{RackManagerApi, RmsApi};
use serde_json::Value;

type Res<T> = Result<T, Box<dyn std::error::Error>>;

fn env(k: &str) -> String {
    std::env::var(k).unwrap_or_else(|_| panic!("{k} is required"))
}

fn check(ok: bool, what: &str) -> Res<()> {
    if ok {
        println!("  ok: {what}");
        Ok(())
    } else {
        Err(format!("FAILED: {what}").into())
    }
}

fn s(v: &Value, k: &str) -> String {
    v[k].as_str().unwrap_or_default().to_string()
}

/// A node as NICo names one before ingestion: its BMC MAC and address, with the BMC credentials.
fn node(v: &Value, id: &str) -> rms::NodeInfo {
    rms::NodeInfo {
        node_id: id.into(),
        rack_id: s(v, "rack"),
        bmc_endpoint: Some(rms::Endpoint {
            interface: Some(rms::NetworkInterface { ip_address: s(v, "siteIP"), mac_address: s(v, "bmcMAC"), host_name: None }),
            port: 443,
            credentials: Some(rms::Credentials {
                auth: Some(rms::credentials::Auth::UserPass(rms::UsernamePassword { username: s(v, "user"), password: s(v, "password") })),
            }),
        }),
        ..Default::default()
    }
}

fn id_of(v: &Value) -> String {
    s(v, "path").rsplit('/').next().unwrap_or_default().to_string()
}

async fn wait_job(api: &RackManagerApi, id: &str) -> Res<rms::JobStatus> {
    for _ in 0..300 {
        let r = api.get_job_status(rms::GetJobStatusRequest { job_id: id.into(), include_child_job_states: true }).await?;
        let j = r.job_states.first().cloned().ok_or("no job state")?;
        if j.execution_state != rms::JobExecutionState::Running as i32 && j.execution_state != rms::JobExecutionState::Queued as i32 {
            return Ok(j);
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    Err(format!("job {id} did not finish").into())
}

async fn power(api: &RackManagerApi, n: &rms::NodeInfo) -> Res<String> {
    let r = api.batch_get_power_state(rms::BatchGetPowerStateRequest { nodes: Some(rms::NodeSet { nodes: vec![n.clone()] }) }).await?;
    Ok(r.node_power_states.first().map(|p| p.pstate.clone()).unwrap_or_default())
}

async fn run() -> Res<()> {
    let url = env("RMS_URL");
    let inv: Vec<Value> = serde_json::from_str(&std::fs::read_to_string(env("RMS_INVENTORY"))?)?;
    let tray_path = env("RMS_TRAY");
    let cfg = RmsClientConfig::new(Some(env("RMS_CA")), None, None, false);
    let api = RackManagerApi::new(&RmsApiConfig::new(&url, &cfg).with_retry_config(RetryConfig { retries: 3, interval: Duration::from_secs(1) }));
    println!("librms RackManagerApi on {url}");

    let v = api.get_version().await?;
    println!("GetVersion: {}", v.version);
    let racks = api.list_racks().await?;
    println!("ListRacks: {:?}", racks.rack_ids);

    let all: Vec<rms::NodeInfo> = inv.iter().map(|v| node(v, &id_of(v))).collect();
    let info = api.batch_get_node_device_info(rms::BatchGetNodeDeviceInfoRequest { nodes: Some(rms::NodeSet { nodes: all.clone() }) }).await?;
    println!(
        "BatchGetNodeDeviceInfo: status {:?} {} of {} nodes placed",
        rms::ReturnCode::try_from(info.status).map(|c| c.as_str_name()).unwrap_or("?"),
        info.node_device_details.len(),
        all.len()
    );
    for d in info.node_device_details.iter().filter(|d| d.node_id.starts_with("compute-tray-0") || d.node_id == "switch-tray-01").take(3) {
        println!("  {} chassis_sn={:?} slot_number={:?} tray_index={:?}", d.node_id, d.chassis_sn, d.slot_number, d.tray_index);
    }
    check(info.status == rms::ReturnCode::Success as i32 && info.node_device_details.len() == all.len(), "every node of the rack placed")?;

    let tray = inv.iter().find(|v| s(v, "path") == tray_path).ok_or("RMS_TRAY is not in the inventory")?;
    let t = node(tray, &id_of(tray));
    println!("BatchGetPowerState {}: {}", t.node_id, power(&api, &t).await?);
    for (op, want) in [(rms::PowerOperation::Off, "OFF"), (rms::PowerOperation::On, "ON")] {
        let r = api
            .batch_set_power_state(rms::BatchSetPowerStateRequest { nodes: Some(rms::NodeSet { nodes: vec![t.clone()] }), operation: op as i32 })
            .await?;
        let b = r.response.unwrap_or_default();
        println!("BatchSetPowerState {} {}: {:?} {}", t.node_id, op.as_str_name(), rms::ReturnCode::try_from(b.status).map(|c| c.as_str_name()), b.message);
        let mut got = String::new();
        for _ in 0..200 {
            got = power(&api, &t).await?;
            if got == want {
                break;
            }
            tokio::time::sleep(Duration::from_millis(100)).await;
        }
        println!("BatchGetPowerState {}: {got}", t.node_id);
        check(got == want, &format!("the tray reads {want} after {}", op.as_str_name()))?;
    }

    let switches: Vec<rms::NodeInfo> = inv.iter().filter(|v| s(v, "class") == "switch-tray").map(|v| node(v, &id_of(v))).collect();
    let job = api
        .configure_scale_up_fabric_manager_v2(rms_v2::ConfigureScaleUpFabricManagerRequest {
            nodes: Some(rms::NodeSet { nodes: switches.clone() }),
            primary_switch_node_id: None,
            domain: None,
            config: Some(rms_v2::ScaleUpFabricConfig { topology_type: "gb300_nvl72r1_c2g4_topology".into(), extra_static_configs: vec![] }),
        })
        .await?;
    println!("ConfigureScaleUpFabricManager (V2): job {}", job.job_id);
    let j = wait_job(&api, &job.job_id).await?;
    println!(
        "GetJobStatus {}: {} — {}",
        j.job_id,
        rms::JobExecutionState::try_from(j.execution_state).map(|s| s.as_str_name()).unwrap_or("?"),
        j.result_json.chars().take(240).collect::<String>()
    );
    check(j.execution_state == rms::JobExecutionState::Completed as i32, "the fabric job completes")?;
    let fs = api.get_scale_up_fabric_status(rms::GetScaleUpFabricStatusRequest { nodes: Some(rms::NodeSet { nodes: switches.clone() }), domain: None }).await?;
    let fs = fs.fabric_status.unwrap_or_default();
    let primary: Vec<_> = fs.switches.iter().filter(|s| s.enabled).map(|s| s.node_id.clone()).collect();
    println!("GetScaleUpFabricStatus: topology {} primary {:?}", fs.topology_type, primary);
    check(primary.len() == 1, "one fabric-manager primary")?;
    let svc = api.batch_get_scale_up_fabric_service_status(rms::BatchGetScaleUpFabricServiceStatusRequest { nodes: Some(rms::NodeSet { nodes: switches.clone() }) }).await?;
    println!("BatchGetScaleUpFabricServiceStatus {}: {}", primary[0], svc.service_statuses[&primary[0]].status_json);

    let objs = api.list_firmware_objects(rms::ListFirmwareObjectsRequest::default()).await?;
    println!("ListFirmwareObjects: {:?}", objs.objects.iter().map(|o| o.id.clone()).collect::<Vec<_>>());
    let mut filters = std::collections::HashMap::new();
    filters.insert(rms::NodeType::ComputeGb300Nvidia as i32, rms::FirmwareObjectComponentFilter { components: vec!["BMC".into()] });
    let applied = api
        .apply_firmware_object(rms::ApplyFirmwareObjectRequest {
            rack_id: s(tray, "rack"),
            config_json: r#"{"Id":"dctwin-check-fw","Version":"25.10.99"}"#.into(),
            access_token: Some("noauth".into()),
            firmware_type: "prod".into(),
            hardware_type: String::new(),
            nodes: Some(rms::NodeSet { nodes: vec![t.clone()] }),
            force_update: false,
            component_filters: filters,
            node_descriptor_component_filters: vec![],
        })
        .await?;
    let b = applied.response.clone().unwrap_or_default();
    println!("ApplyFirmwareObject {}: parent job {} child jobs {:?}", applied.object_id, b.job_id, applied.jobs.iter().map(|j| j.job_id.clone()).collect::<Vec<_>>());
    let child = applied.jobs.first().ok_or("no child job")?.job_id.clone();
    let mut last = rms::GetFirmwareJobStatusResponse::default();
    for _ in 0..300 {
        last = api.get_firmware_job_status(rms::GetFirmwareJobStatusRequest { job_id: child.clone() }).await?;
        if last.job_state != rms::FirmwareJobState::Running as i32 && last.job_state != rms::FirmwareJobState::Queued as i32 {
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    println!(
        "GetFirmwareJobStatus {child}: {} {}",
        rms::FirmwareJobState::try_from(last.job_state).map(|s| s.as_str_name()).unwrap_or("?"),
        last.result_json
    );
    check(last.job_state == rms::FirmwareJobState::Completed as i32, "the firmware job completes")?;

    let img = api
        .apply_switch_system_image(rms::ApplySwitchSystemImageRequest {
            rack_id: s(tray, "rack"),
            config_json: r#"{"Id":"dctwin-check-nvos","Version":"25.02.9999"}"#.into(),
            access_token: Some("noauth".into()),
            software_type: "prod".into(),
            hardware_type: String::new(),
            nodes: Some(rms::NodeSet { nodes: switches[..1].to_vec() }),
        })
        .await?;
    let img_job = img.response.clone().unwrap_or_default().job_id;
    let mut state = String::new();
    for _ in 0..300 {
        state = api.get_switch_system_image_job_status(rms::GetSwitchSystemImageJobStatusRequest { job_id: img_job.clone() }).await?.state;
        if state != "running" && state != "queued" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    println!("ApplySwitchSystemImage {} ({}): job {img_job} {state}", img.object_id, img.image_filename);
    check(state == "completed", "the switch image job completes")?;

    let cert = api
        .configure_switch_certificate(rms::ConfigureSwitchCertificateRequest {
            nodes: Some(rms::NodeSet { nodes: switches.clone() }),
            services: vec![rms::SwitchService::NvueApi as i32, rms::SwitchService::ScaleUpFabricManager as i32],
            test_hello: true,
            domain: None,
        })
        .await?;
    let cert_job = cert.response.clone().unwrap_or_default().job_id;
    let mut cstate = String::new();
    for _ in 0..300 {
        cstate = api.get_configure_switch_certificate_job_status(rms::GetConfigureSwitchCertificateJobStatusRequest { job_id: cert_job.clone() }).await?.state;
        if cstate != "running" && cstate != "queued" {
            break;
        }
        tokio::time::sleep(Duration::from_millis(250)).await;
    }
    println!("ConfigureSwitchCertificate on {} switches: job {cert_job} {cstate} ({} child jobs)", switches.len(), cert.jobs.len());
    check(cstate == "completed", "the certificate job completes")?;
    Ok(())
}

#[tokio::main]
async fn main() -> ExitCode {
    match run().await {
        Ok(()) => {
            println!("rms_check: PASS");
            ExitCode::SUCCESS
        }
        Err(e) => {
            println!("rms_check: {e}");
            ExitCode::FAILURE
        }
    }
}
