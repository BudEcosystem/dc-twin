package redfish

import (
	"fmt"
	"sort"
	"strings"

	"github.com/BudEcosystem/dc-twin/internal/model"
)

// generatedBackend builds a DMTF-schema tree from the model for profiles with no captured
// service (generic servers, PDUs). It answers only the resources below; anything else is 404.
type generatedBackend struct{}

func odata(path, typ string) map[string]any {
	return map[string]any{"@odata.id": path, "@odata.type": typ}
}

func ref(path string) map[string]any { return map[string]any{"@odata.id": path} }

func collection(path, typ, name string, members []string) map[string]any {
	m := odata(path, typ)
	m["Name"] = name
	refs := []any{}
	for _, x := range members {
		refs = append(refs, ref(x))
	}
	m["Members"] = refs
	m["Members@odata.count"] = len(refs)
	return m
}

func status(health, state string) map[string]any {
	return map[string]any{"Health": health, "HealthRollup": health, "State": state}
}

func (generatedBackend) render(rd *renderer, path string) (map[string]any, bool) {
	if doc, ok := generatedDynamic(rd, path); ok {
		return doc, true
	}
	if doc, ok := rd.dynamic(path); ok {
		return doc, true
	}
	b, s := rd.b, rd.snap
	d := b.dev
	idsp := d.Profile.Spec.Identity
	manuf, modelName := "dc-twin", d.ProfileName
	if idsp != nil {
		if idsp.Manufacturer != "" {
			manuf = idsp.Manufacturer
		}
		if idsp.Model != "" {
			modelName = idsp.Model
		}
	}
	isPDU := d.BaseClass == "pdu"
	sys, ch, mgr := "/redfish/v1/Systems/1", "/redfish/v1/Chassis/1", "/redfish/v1/Managers/1"
	state := redfishState(s.power, s.ac)
	var doc map[string]any
	switch path {
	case "/redfish/v1":
		doc = odata(path, "#ServiceRoot.v1_15_0.ServiceRoot")
		doc["Id"], doc["Name"], doc["RedfishVersion"], doc["UUID"] = "RootService", "Root Service", "1.17.0", d.Identity.UUID
		for k, v := range b.dialect.Spec.ServiceRoot {
			doc[k] = v
		}
		if !isPDU {
			doc["Systems"] = ref("/redfish/v1/Systems")
		} else {
			doc["PowerEquipment"] = ref("/redfish/v1/PowerEquipment")
		}
		doc["Chassis"], doc["Managers"] = ref("/redfish/v1/Chassis"), ref("/redfish/v1/Managers")
		doc["AccountService"], doc["SessionService"] = ref("/redfish/v1/AccountService"), ref("/redfish/v1/SessionService")
		// ServiceRoot links the task service as "Tasks" (it has no "TaskService" property)
		doc["UpdateService"], doc["Tasks"], doc["EventService"] = ref("/redfish/v1/UpdateService"), ref("/redfish/v1/TaskService"), ref("/redfish/v1/EventService")
		doc["Links"] = map[string]any{"Sessions": ref("/redfish/v1/SessionService/Sessions")}
	case "/redfish/v1/odata":
		doc = map[string]any{"@odata.context": "/redfish/v1/$metadata", "value": []any{
			map[string]any{"name": "Service", "kind": "Singleton", "url": "/redfish/v1/"},
			map[string]any{"name": "Chassis", "kind": "Singleton", "url": "/redfish/v1/Chassis"},
			map[string]any{"name": "Managers", "kind": "Singleton", "url": "/redfish/v1/Managers"}}}
	case "/redfish/v1/Systems":
		if isPDU {
			return nil, false
		}
		doc = collection(path, "#ComputerSystemCollection.ComputerSystemCollection", "Computer System Collection", []string{sys})
	case sys:
		if isPDU {
			return nil, false
		}
		doc = odata(path, "#ComputerSystem.v1_20_0.ComputerSystem")
		doc["Id"], doc["Name"], doc["SystemType"] = "1", "System", "Physical"
		doc["Manufacturer"], doc["Model"], doc["SerialNumber"], doc["UUID"] = manuf, modelName, d.Identity.Serial, d.Identity.UUID
		doc["PowerState"], doc["Status"] = redfishPower(s.power), status(s.health, state)
		last, at := bootProgress(s)
		doc["BootProgress"] = map[string]any{"LastState": last, "LastStateTime": at}
		if v := s.firmware["bios"]; v != "" {
			doc["BiosVersion"] = v
		}
		cpus, gpus, mem := 0, 0, 0
		for _, c := range d.Components {
			switch c.Kind {
			case "cpu":
				cpus++
			case "accelerator":
				gpus++
			case "memory":
				mem++
			}
		}
		doc["ProcessorSummary"] = map[string]any{"Count": cpus, "Status": status(s.health, state)}
		doc["MemorySummary"] = map[string]any{"TotalSystemMemoryGiB": maxInt(mem, 1) * 64, "Status": status(model.OK, "Enabled")}
		order := []any{}
		for _, o := range s.boot.Order {
			order = append(order, o)
		}
		doc["Boot"] = map[string]any{"BootSourceOverrideTarget": s.boot.OverrideTarget, "BootSourceOverrideEnabled": s.boot.OverrideEnabled,
			"BootSourceOverrideMode": orDefault(s.boot.OverrideMode, "UEFI"), "BootOrder": order, "HttpBootUri": s.boot.HTTPBootURI,
			"BootSourceOverrideTarget@Redfish.AllowableValues": []any{"None", "Pxe", "Hdd", "Cd", "BiosSetup", "UefiHttp", "UefiShell"},
			"BootOptions": ref(sys + "/BootOptions")}
		doc["Processors"], doc["Memory"], doc["EthernetInterfaces"] = ref(sys+"/Processors"), ref(sys+"/Memory"), ref(sys+"/EthernetInterfaces")
		doc["Storage"], doc["LogServices"], doc["Bios"], doc["SecureBoot"] = ref(sys+"/Storage"), ref(sys+"/LogServices"), ref(sys+"/Bios"), ref(sys+"/SecureBoot")
		doc["Actions"] = map[string]any{"#ComputerSystem.Reset": map[string]any{"target": sys + "/Actions/ComputerSystem.Reset",
			"ResetType@Redfish.AllowableValues": []any{"On", "ForceOff", "GracefulShutdown", "GracefulRestart", "ForceRestart", "PowerCycle", "Nmi"}}}
		doc["Links"] = map[string]any{"Chassis": []any{ref(ch)}, "ManagedBy": []any{ref(mgr)}}
	case sys + "/Processors":
		var ids []string
		for _, c := range d.Components {
			if c.Kind == "cpu" || c.Kind == "accelerator" {
				ids = append(ids, sys+"/Processors/"+procID(c))
			}
		}
		doc = collection(path, "#ProcessorCollection.ProcessorCollection", "Processors Collection", ids)
	case sys + "/Memory":
		var ids []string
		for _, c := range d.Components {
			if c.Kind == "memory" {
				ids = append(ids, sys+"/Memory/"+strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_")))
			}
		}
		doc = collection(path, "#MemoryCollection.MemoryCollection", "Memory Collection", ids)
	case sys + "/EthernetInterfaces":
		var ids []string
		for _, p := range d.Ports {
			if p.Kind == "ethernet" && p.Role != "management" && p.Component == nil {
				ids = append(ids, sys+"/EthernetInterfaces/"+p.Name)
			}
		}
		doc = collection(path, "#EthernetInterfaceCollection.EthernetInterfaceCollection", "Ethernet Interfaces", ids)
	case sys + "/Storage":
		var ids []string
		if len(drives(d)) > 0 {
			ids = []string{sys + "/Storage/1"}
		}
		doc = collection(path, "#StorageCollection.StorageCollection", "Storage", ids)
	case sys + "/Storage/1":
		ds := drives(d)
		if len(ds) == 0 {
			return nil, false
		}
		doc = odata(path, "#Storage.v1_15_0.Storage")
		var refs []any
		for _, c := range ds {
			refs = append(refs, ref(sys+"/Storage/1/Drives/"+driveID(c)))
		}
		doc["Id"], doc["Name"], doc["Drives"], doc["Drives@odata.count"], doc["Status"] = "1", "Storage", refs, len(refs), status(s.health, "Enabled")
	case sys + "/BootOptions":
		var ids []string
		for i := range s.boot.Order {
			ids = append(ids, fmt.Sprintf("%s/BootOptions/%d", sys, i+1))
		}
		doc = collection(path, "#BootOptionCollection.BootOptionCollection", "Boot Options", ids)
	case sys + "/LogServices":
		doc = collection(path, "#LogServiceCollection.LogServiceCollection", "Log Services", []string{sys + "/LogServices/SEL"})
	case sys + "/LogServices/SEL":
		doc = odata(path, "#LogService.v1_6_0.LogService")
		doc["Id"], doc["Name"], doc["Entries"] = "SEL", "System Event Log", ref(path+"/Entries")
		doc["OverWritePolicy"] = "WrapsWhenFull"
		doc["Actions"] = map[string]any{"#LogService.ClearLog": map[string]any{"target": path + "/Actions/LogService.ClearLog"}}
	case sys + "/Bios":
		doc = odata(path, "#Bios.v1_2_1.Bios")
		attrs := map[string]any{"BootMode": "Uefi", "SriovEnable": "Enabled", "VTdSupport": "Enabled", "ProcVirtualization": "Enabled",
			"SecureBootMode": "UserMode", "TpmSecurity": "On", "IPv4PXESupport": "Enabled", "IPv6PXESupport": "Disabled", "HttpBootSupport": "Enabled"}
		if s.biosPending["__reset__"] == nil {
			for k, v := range s.bios {
				attrs[k] = v
			}
		}
		doc["Id"], doc["Name"], doc["AttributeRegistry"], doc["Attributes"] = "Bios", "BIOS Configuration", "BiosAttributeRegistry.v1_0_0", attrs
		doc["@Redfish.Settings"] = map[string]any{"@odata.type": "#Settings.v1_3_5.Settings", "SettingsObject": ref(path + "/Settings")}
		doc["Actions"] = map[string]any{"#Bios.ResetBios": map[string]any{"target": path + "/Actions/Bios.ResetBios"}}
	case sys + "/Bios/Settings":
		doc = odata(path, "#Bios.v1_2_1.Bios")
		attrs := map[string]any{}
		for k, v := range s.biosPending {
			if k != "__reset__" {
				attrs[k] = v
			}
		}
		doc["Id"], doc["Name"], doc["Attributes"] = "Settings", "BIOS Pending Settings", attrs
	case sys + "/SecureBoot":
		doc = odata(path, "#SecureBoot.v1_1_0.SecureBoot")
		cur := "Disabled"
		if s.secureBoot {
			cur = "Enabled"
		}
		doc["Id"], doc["Name"], doc["SecureBootEnable"], doc["SecureBootCurrentBoot"], doc["SecureBootMode"] = "SecureBoot", "UEFI Secure Boot", s.secureBoot, cur, "UserMode"
	case "/redfish/v1/Chassis":
		doc = collection(path, "#ChassisCollection.ChassisCollection", "Chassis Collection", []string{ch})
	case ch:
		doc = odata(path, "#Chassis.v1_23_0.Chassis")
		ctype := "RackMount"
		if isPDU {
			ctype = "Zone"
		}
		doc["Id"], doc["Name"], doc["ChassisType"] = "1", "Chassis", ctype
		doc["Manufacturer"], doc["Model"], doc["SerialNumber"] = manuf, modelName, d.Identity.Serial
		if idsp != nil && idsp.PartNumber != "" {
			doc["PartNumber"] = idsp.PartNumber
		}
		doc["PowerState"], doc["Status"] = redfishPower(s.power), status(s.health, state)
		doc["Power"], doc["Thermal"], doc["Sensors"] = ref(ch+"/Power"), ref(ch+"/Thermal"), ref(ch+"/Sensors")
		doc["PowerSubsystem"], doc["ThermalSubsystem"] = ref(ch+"/PowerSubsystem"), ref(ch+"/ThermalSubsystem")
		links := map[string]any{"ManagedBy": []any{ref(mgr)}}
		if !isPDU {
			links["ComputerSystems"] = []any{ref(sys)}
		}
		doc["Links"] = links
	case ch + "/Power":
		doc = odata(path, "#Power.v1_7_3.Power")
		doc["Id"], doc["Name"] = "Power", "Power"
		doc["PowerControl"] = []any{map[string]any{"@odata.id": path + "#/PowerControl/0", "MemberId": "0", "Name": "System Power Control",
			"PowerConsumedWatts": s.sensors["power"], "PowerCapacityWatts": d.Profile.Spec.Power.MaxW}}
		psus := []any{}
		for _, c := range d.Components {
			if c.Kind == "psu" {
				h, st := componentStatus(s, c)
				psus = append(psus, map[string]any{"@odata.id": fmt.Sprintf("%s#/PowerSupplies/%d", path, c.Index), "MemberId": fmt.Sprint(c.Index),
					"Name": c.Name, "Model": c.Model, "Status": status(h, st)})
			}
		}
		doc["PowerSupplies"] = psus
	case ch + "/Thermal":
		doc = odata(path, "#Thermal.v1_7_1.Thermal")
		doc["Id"], doc["Name"] = "Thermal", "Thermal"
		temps, fans := []any{}, []any{}
		i := 0
		for _, sp := range d.Profile.Spec.Sensors {
			if sp.Quantity != "temperature" {
				continue
			}
			t := map[string]any{"@odata.id": fmt.Sprintf("%s#/Temperatures/%d", path, i), "MemberId": fmt.Sprint(i), "Name": sp.Name,
				"ReadingCelsius": s.sensors[sp.Name], "Status": status(model.OK, "Enabled")}
			if sp.Warn != nil {
				t["UpperThresholdNonCritical"] = *sp.Warn
			}
			if sp.Crit != nil {
				t["UpperThresholdCritical"] = *sp.Crit
			}
			temps = append(temps, t)
			i++
		}
		for _, c := range d.Components {
			if c.Kind == "fan" {
				h, st := componentStatus(s, c)
				fans = append(fans, map[string]any{"@odata.id": fmt.Sprintf("%s#/Fans/%d", path, c.Index), "MemberId": fmt.Sprint(c.Index),
					"Name": c.Name, "Reading": 9000, "ReadingUnits": "RPM", "Status": status(h, st)})
			}
		}
		doc["Temperatures"], doc["Fans"] = temps, fans
	case ch + "/PowerSubsystem":
		doc = odata(path, "#PowerSubsystem.v1_1_0.PowerSubsystem")
		doc["Id"], doc["Name"], doc["Status"] = "PowerSubsystem", "Power Subsystem", status(s.health, state)
		doc["CapacityWatts"] = d.Profile.Spec.Power.MaxW
	case ch + "/ThermalSubsystem":
		doc = odata(path, "#ThermalSubsystem.v1_3_0.ThermalSubsystem")
		doc["Id"], doc["Name"], doc["Status"] = "ThermalSubsystem", "Thermal Subsystem", status(s.health, state)
		if hasLeak(d) {
			doc["LeakDetection"] = ref(ch + "/ThermalSubsystem/LeakDetection")
		}
	case ch + "/ThermalSubsystem/LeakDetection":
		if !hasLeak(d) {
			return nil, false
		}
		doc = odata(path, "#LeakDetection.v1_1_0.LeakDetection")
		doc["Id"], doc["Name"], doc["LeakDetectors"] = "LeakDetection", "Leak Detection", ref(path+"/LeakDetectors")
	case ch + "/ThermalSubsystem/LeakDetection/LeakDetectors":
		if !hasLeak(d) {
			return nil, false
		}
		doc = collection(path, "#LeakDetectorCollection.LeakDetectorCollection", "Leak Detectors", []string{path + "/LeakDetector_0"})
	case ch + "/ThermalSubsystem/LeakDetection/LeakDetectors/LeakDetector_0":
		if !hasLeak(d) {
			return nil, false
		}
		st := "OK"
		if s.sensors["leak"] >= 1 {
			st = "Critical"
		}
		doc = odata(path, "#LeakDetector.v1_1_0.LeakDetector")
		doc["Id"], doc["Name"], doc["LeakDetectorType"], doc["DetectorState"], doc["Status"] = "LeakDetector_0", "Leak Detector 0", "Moisture", st, map[string]any{"Health": st, "State": "Enabled"}
	case ch + "/Sensors":
		var ids []string
		for _, sp := range d.Profile.Spec.Sensors {
			ids = append(ids, ch+"/Sensors/"+sp.Name)
		}
		doc = collection(path, "#SensorCollection.SensorCollection", "Sensors", ids)
	case mgr[:len("/redfish/v1/Managers")]:
		doc = collection(path, "#ManagerCollection.ManagerCollection", "Manager Collection", []string{mgr})
	case mgr:
		doc = odata(path, "#Manager.v1_19_0.Manager")
		doc["Id"], doc["Name"], doc["ManagerType"], doc["Status"] = "1", "Manager", "BMC", status(model.OK, "Enabled")
		doc["UUID"] = d.Identity.UUID
		doc["DateTimeLocalOffset"] = "+00:00"
		doc["DateTime"] = bmcDateTime(s, doc["DateTimeLocalOffset"])
		if v := s.firmware["bmc"]; v != "" { // a device with no BMC firmware omits it, as DMTF asks
			doc["FirmwareVersion"] = v
		}
		doc["EthernetInterfaces"], doc["NetworkProtocol"], doc["LogServices"] = ref(mgr+"/EthernetInterfaces"), ref(mgr+"/NetworkProtocol"), ref(mgr+"/LogServices")
		doc["Actions"] = map[string]any{"#Manager.Reset": map[string]any{"target": mgr + "/Actions/Manager.Reset", "ResetType@Redfish.AllowableValues": []any{"GracefulRestart", "ForceRestart"}}}
		links := map[string]any{"ManagerForChassis": []any{ref(ch)}}
		if !isPDU {
			links["ManagerForServers"] = []any{ref(sys)}
		}
		doc["Links"] = links
	case mgr + "/EthernetInterfaces":
		doc = collection(path, "#EthernetInterfaceCollection.EthernetInterfaceCollection", "Manager Ethernet Interfaces", []string{mgr + "/EthernetInterfaces/1"})
	case mgr + "/EthernetInterfaces/1":
		doc = odata(path, "#EthernetInterface.v1_12_0.EthernetInterface")
		doc["Id"], doc["Name"], doc["MACAddress"], doc["PermanentMACAddress"], doc["HostName"] = "1", "Manager Ethernet Interface", d.Identity.BMCMAC, d.Identity.BMCMAC, b.hostname
		doc["InterfaceEnabled"], doc["LinkStatus"], doc["SpeedMbps"] = true, "LinkUp", 1000
		doc["IPv4Addresses"] = rd.addresses("IPv4Addresses", b.dialect.Spec.BMCPort, nil)
		doc["Status"] = status(model.OK, "Enabled")
	case mgr + "/NetworkProtocol":
		doc = odata(path, "#ManagerNetworkProtocol.v1_10_0.ManagerNetworkProtocol")
		doc["Id"], doc["Name"], doc["HostName"] = "NetworkProtocol", "Manager Network Protocol", b.hostname
		doc["HTTPS"] = map[string]any{"ProtocolEnabled": true, "Port": 443}
		doc["IPMI"] = map[string]any{"ProtocolEnabled": d.Profile.Spec.BMC != nil && d.Profile.Spec.BMC.IPMI, "Port": 623}
		doc["SSH"] = map[string]any{"ProtocolEnabled": true, "Port": 22}
	case mgr + "/LogServices":
		doc = collection(path, "#LogServiceCollection.LogServiceCollection", "Log Services", nil)
	case "/redfish/v1/AccountService":
		doc = odata(path, "#AccountService.v1_15_0.AccountService")
		doc["Id"], doc["Name"], doc["ServiceEnabled"], doc["MinPasswordLength"], doc["MaxPasswordLength"] = "AccountService", "Account Service", true, 8, 20
		doc["Accounts"], doc["Roles"] = ref(path+"/Accounts"), ref(path+"/Roles")
	case "/redfish/v1/AccountService/Roles":
		doc = collection(path, "#RoleCollection.RoleCollection", "Roles", []string{path + "/Administrator", path + "/Operator", path + "/ReadOnly"})
	case "/redfish/v1/AccountService/Roles/Administrator", "/redfish/v1/AccountService/Roles/Operator", "/redfish/v1/AccountService/Roles/ReadOnly":
		id := path[strings.LastIndex(path, "/")+1:]
		doc = odata(path, "#Role.v1_3_1.Role")
		doc["Id"], doc["Name"], doc["RoleId"], doc["IsPredefined"] = id, id, id, true
		doc["AssignedPrivileges"] = map[string][]any{"Administrator": {"Login", "ConfigureManager", "ConfigureUsers", "ConfigureSelf", "ConfigureComponents"},
			"Operator": {"Login", "ConfigureSelf", "ConfigureComponents"}, "ReadOnly": {"Login", "ConfigureSelf"}}[id]
	case "/redfish/v1/SessionService":
		doc = odata(path, "#SessionService.v1_1_9.SessionService")
		doc["Id"], doc["Name"], doc["ServiceEnabled"], doc["SessionTimeout"], doc["Sessions"] = "SessionService", "Session Service", true, 1800, ref(path+"/Sessions")
	case "/redfish/v1/UpdateService":
		doc = odata(path, "#UpdateService.v1_14_0.UpdateService")
		doc["Id"], doc["Name"], doc["ServiceEnabled"] = "UpdateService", "Update Service", true
		doc["FirmwareInventory"], doc["MultipartHttpPushUri"] = ref(path+"/FirmwareInventory"), path+"/upload"
		doc["Actions"] = map[string]any{"#UpdateService.SimpleUpdate": map[string]any{"target": path + "/Actions/UpdateService.SimpleUpdate",
			"TransferProtocol@Redfish.AllowableValues": []any{"HTTP", "HTTPS"}}}
	case "/redfish/v1/UpdateService/FirmwareInventory":
		var ids []string
		for _, k := range sortedKeys(s.firmware) {
			ids = append(ids, path+"/"+k)
		}
		doc = collection(path, "#SoftwareInventoryCollection.SoftwareInventoryCollection", "Firmware Inventory", ids)
	case "/redfish/v1/TaskService":
		doc = odata(path, "#TaskService.v1_2_0.TaskService")
		doc["Id"], doc["Name"], doc["ServiceEnabled"], doc["Tasks"] = "TaskService", "Task Service", true, ref(path+"/Tasks")
	case "/redfish/v1/EventService":
		doc = odata(path, "#EventService.v1_10_0.EventService")
		doc["Id"], doc["Name"], doc["ServiceEnabled"], doc["Subscriptions"] = "EventService", "Event Service", true, ref(path+"/Subscriptions")
		doc["ServerSentEventUri"] = "/redfish/v1/EventService/SSE"
		doc["EventFormatTypes"] = []any{"Event"}
		doc["Actions"] = map[string]any{"#EventService.SubmitTestEvent": map[string]any{"target": path + "/Actions/EventService.SubmitTestEvent"}}
	case "/redfish/v1/PowerEquipment":
		if !isPDU {
			return nil, false
		}
		doc = odata(path, "#PowerEquipment.v1_2_0.PowerEquipment")
		doc["Id"], doc["Name"], doc["RackPDUs"] = "PowerEquipment", "Power Equipment", ref(path+"/RackPDUs")
	case "/redfish/v1/PowerEquipment/RackPDUs":
		if !isPDU {
			return nil, false
		}
		doc = collection(path, "#PowerDistributionCollection.PowerDistributionCollection", "Rack PDUs", []string{path + "/1"})
	case "/redfish/v1/PowerEquipment/RackPDUs/1":
		if !isPDU {
			return nil, false
		}
		doc = odata(path, "#PowerDistribution.v1_3_0.PowerDistribution")
		doc["Id"], doc["Name"], doc["EquipmentType"], doc["Manufacturer"], doc["Model"], doc["SerialNumber"] = "1", "Rack PDU", "RackPDU", manuf, modelName, d.Identity.Serial
		doc["Status"], doc["Outlets"] = status(s.health, state), ref(path+"/Outlets")
	case "/redfish/v1/PowerEquipment/RackPDUs/1/Outlets":
		if !isPDU || d.Profile.Spec.SNMP == nil {
			return nil, false
		}
		var ids []string
		for i := 1; i <= d.Profile.Spec.SNMP.Outlets; i++ {
			ids = append(ids, fmt.Sprintf("%s/%d", path, i))
		}
		doc = collection(path, "#OutletCollection.OutletCollection", "Outlets", ids)
	}
	if doc != nil {
		return doc, true
	}
	// parameterised resources
	switch {
	case strings.HasPrefix(path, sys+"/Processors/"):
		id := strings.TrimPrefix(path, sys+"/Processors/")
		for _, c := range d.Components {
			if (c.Kind == "cpu" || c.Kind == "accelerator") && procID(c) == id {
				h, st := componentStatus(s, c)
				doc = odata(path, "#Processor.v1_18_0.Processor")
				ptype := "CPU"
				if c.Kind == "accelerator" {
					ptype = "GPU"
				}
				doc["Id"], doc["Name"], doc["ProcessorType"], doc["Model"], doc["Manufacturer"], doc["Status"] = id, c.Name, ptype, c.Model, orDefault(c.Vendor, manuf), status(h, st)
				if c.Kind == "cpu" {
					// What an inspector reads for a node's cpus and cpu_arch (Ironic does).
					cores := model.CPUCores(c)
					doc["TotalCores"], doc["TotalThreads"] = cores, cores*2
					if model.HostArch(d) == "arm64" {
						doc["ProcessorArchitecture"], doc["InstructionSet"] = "ARM", "ARM-A64"
						doc["TotalThreads"] = cores
					} else {
						doc["ProcessorArchitecture"], doc["InstructionSet"] = "x86", "x86-64"
					}
				}
				return doc, true
			}
		}
	case strings.HasPrefix(path, sys+"/Storage/1/Drives/"):
		id := strings.TrimPrefix(path, sys+"/Storage/1/Drives/")
		for _, c := range drives(d) {
			if driveID(c) == id {
				h, st := componentStatus(s, c)
				doc = odata(path, "#Drive.v1_17_0.Drive")
				doc["Id"], doc["Name"], doc["Model"], doc["Status"] = id, c.Name, c.Model, status(h, st)
				doc["MediaType"], doc["CapacityBytes"] = "SSD", int64(model.DriveBytes(c))
				if strings.Contains(strings.ToUpper(c.Model), "NVME") {
					doc["Protocol"] = "NVMe"
				} else {
					doc["Protocol"] = "SATA"
				}
				return doc, true
			}
		}
	case strings.HasPrefix(path, sys+"/Memory/"):
		id := strings.TrimPrefix(path, sys+"/Memory/")
		for _, c := range d.Components {
			if c.Kind == "memory" && strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_")) == id {
				h, st := componentStatus(s, c)
				doc = odata(path, "#Memory.v1_19_0.Memory")
				doc["Id"], doc["Name"], doc["MemoryDeviceType"], doc["CapacityMiB"], doc["Status"] = id, c.Name, "DDR5", 65536, status(h, st)
				return doc, true
			}
		}
	case strings.HasPrefix(path, sys+"/EthernetInterfaces/"):
		name := strings.TrimPrefix(path, sys+"/EthernetInterfaces/")
		if p := d.PortBy[name]; p != nil && p.Kind == "ethernet" {
			doc = odata(path, "#EthernetInterface.v1_12_0.EthernetInterface")
			link := "LinkDown"
			if b.a.eng.PortUp(p) {
				link = "LinkUp"
			}
			doc["Id"], doc["Name"], doc["MACAddress"], doc["PermanentMACAddress"], doc["LinkStatus"] = name, name, p.MAC, p.MAC, link
			doc["InterfaceEnabled"], doc["Status"] = true, status(model.OK, "Enabled")
			return doc, true
		}
	case strings.HasPrefix(path, sys+"/BootOptions/"):
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(path, sys+"/BootOptions/"), "%d", &n); err == nil && n >= 1 && n <= len(s.boot.Order) {
			doc = odata(path, "#BootOption.v1_0_4.BootOption")
			doc["Id"], doc["Name"], doc["BootOptionReference"], doc["DisplayName"] = fmt.Sprint(n), s.boot.Order[n-1], s.boot.Order[n-1], s.boot.Order[n-1]
			return doc, true
		}
	case strings.HasPrefix(path, ch+"/Sensors/"):
		name := strings.TrimPrefix(path, ch+"/Sensors/")
		for _, sp := range d.Profile.Spec.Sensors {
			if sp.Name != name {
				continue
			}
			doc = odata(path, "#Sensor.v1_7_0.Sensor")
			rtype, units := "Temperature", "Cel"
			switch sp.Quantity {
			case "power":
				rtype, units = "Power", "W"
			case "current":
				rtype, units = "Current", "A"
			case "flow":
				rtype, units = "LiquidFlowLPM", "L/min"
			case "leak", "state":
				rtype, units = "Percent", "%"
			}
			doc["Id"], doc["Name"], doc["ReadingType"], doc["ReadingUnits"], doc["Reading"], doc["Status"] = name, name, rtype, units, s.sensors[name], status(model.OK, "Enabled")
			return doc, true
		}
	case strings.HasPrefix(path, "/redfish/v1/UpdateService/FirmwareInventory/"):
		id := strings.TrimPrefix(path, "/redfish/v1/UpdateService/FirmwareInventory/")
		if v, ok := s.firmware[id]; ok {
			doc = odata(path, "#SoftwareInventory.v1_10_0.SoftwareInventory")
			doc["Id"], doc["Name"], doc["Version"], doc["Updateable"], doc["Status"] = id, id, v, true, status(model.OK, "Enabled")
			return doc, true
		}
	case strings.HasPrefix(path, "/redfish/v1/PowerEquipment/RackPDUs/1/Outlets/") && isPDU:
		var n int
		if _, err := fmt.Sscanf(strings.TrimPrefix(path, "/redfish/v1/PowerEquipment/RackPDUs/1/Outlets/"), "%d", &n); err == nil {
			on := true
			b.a.eng.Read(func(*model.Model) {
				if v, ok := d.State.Outlets[n]; ok {
					on = v
				}
			})
			ps := "On"
			if !on {
				ps = "Off"
			}
			doc = odata(path, "#Outlet.v1_4_3.Outlet")
			doc["Id"], doc["Name"], doc["PowerState"], doc["Status"] = fmt.Sprint(n), fmt.Sprintf("Outlet %d", n), ps, status(model.OK, "Enabled")
			doc["Actions"] = map[string]any{"#Outlet.PowerControl": map[string]any{"target": path + "/Actions/Outlet.PowerControl"}}
			return doc, true
		}
	}
	return nil, false
}

// generatedDynamic answers the model-state resources whose captured-tree rendering
// (renderer.dynamic) starts from the capture: the accounts and the system event log's entries.
// A generated BMC has no captured tree to start from.
func generatedDynamic(rd *renderer, path string) (map[string]any, bool) {
	const accounts = "/redfish/v1/AccountService/Accounts"
	s := rd.snap
	switch {
	case path == accounts:
		var ids []string
		for _, a := range s.accounts {
			ids = append(ids, accounts+"/"+a.ID)
		}
		return collection(path, "#ManagerAccountCollection.ManagerAccountCollection", "Accounts Collection", ids), true
	case strings.HasPrefix(path, accounts+"/"):
		id := strings.TrimPrefix(path, accounts+"/")
		for _, a := range s.accounts {
			if a.ID != id {
				continue
			}
			doc := odata(path, "#ManagerAccount.v1_10_0.ManagerAccount")
			doc["Id"], doc["Name"], doc["UserName"], doc["RoleId"], doc["Enabled"], doc["Locked"] = a.ID, "User Account", a.User, a.Role, a.Enabled, a.Locked
			doc["Password"], doc["PasswordChangeRequired"], doc["AccountTypes"] = nil, a.PasswordChangeRequired, []any{"Redfish"}
			if a.Role != "" {
				doc["Links"] = map[string]any{"Role": ref("/redfish/v1/AccountService/Roles/" + a.Role)}
			}
			return doc, true
		}
		return nil, true
	}
	if base, entry, ok := rd.b.logPath(path); ok {
		if entry != "" {
			return rd.logResource(path, base, entry)
		}
		// LogEntryCollection's Members are auto-expanded (OData.AutoExpand): whole entries, not links
		members := []any{}
		for _, l := range s.logs {
			id := fmt.Sprintf("twin-%d", l.ID)
			members = append(members, logEntry(base+"/"+id, id, l))
		}
		doc := odata(path, "#LogEntryCollection.LogEntryCollection")
		doc["Name"], doc["Members"], doc["Members@odata.count"] = "Log Entries", members, len(members)
		return doc, true
	}
	return nil, false
}

func drives(d *model.Device) []*model.Component {
	var out []*model.Component
	for _, c := range d.Components {
		if c.Kind == "drive" {
			out = append(out, c)
		}
	}
	return out
}

func driveID(c *model.Component) string {
	return strings.ToUpper(strings.ReplaceAll(c.Name, "-", "_"))
}

func procID(c *model.Component) string {
	if c.Kind == "accelerator" {
		return fmt.Sprintf("GPU_%d", c.Index)
	}
	return fmt.Sprintf("CPU_%d", c.Index)
}

func componentStatus(s snapshot, c *model.Component) (string, string) {
	for _, x := range s.comps {
		if x.name == c.Name {
			return x.health, x.state
		}
	}
	return model.OK, "Enabled"
}

func hasLeak(d *model.Device) bool {
	for _, s := range d.Profile.Spec.Sensors {
		if s.Source == "tray-leak-detector" {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]string) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

const generatedMetadata = `<?xml version="1.0" encoding="UTF-8"?>
<edmx:Edmx xmlns:edmx="http://docs.oasis-open.org/odata/ns/edmx" Version="4.0">
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ServiceRoot_v1.xml"><edmx:Include Namespace="ServiceRoot"/><edmx:Include Namespace="ServiceRoot.v1_15_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ComputerSystemCollection_v1.xml"><edmx:Include Namespace="ComputerSystemCollection"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ComputerSystem_v1.xml"><edmx:Include Namespace="ComputerSystem"/><edmx:Include Namespace="ComputerSystem.v1_20_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ChassisCollection_v1.xml"><edmx:Include Namespace="ChassisCollection"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/Chassis_v1.xml"><edmx:Include Namespace="Chassis"/><edmx:Include Namespace="Chassis.v1_23_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/ManagerCollection_v1.xml"><edmx:Include Namespace="ManagerCollection"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/Manager_v1.xml"><edmx:Include Namespace="Manager"/><edmx:Include Namespace="Manager.v1_19_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/AccountService_v1.xml"><edmx:Include Namespace="AccountService"/><edmx:Include Namespace="AccountService.v1_15_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/SessionService_v1.xml"><edmx:Include Namespace="SessionService"/><edmx:Include Namespace="SessionService.v1_1_9"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/UpdateService_v1.xml"><edmx:Include Namespace="UpdateService"/><edmx:Include Namespace="UpdateService.v1_14_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/TaskService_v1.xml"><edmx:Include Namespace="TaskService"/><edmx:Include Namespace="TaskService.v1_2_0"/></edmx:Reference>
  <edmx:Reference Uri="http://redfish.dmtf.org/schemas/v1/EventService_v1.xml"><edmx:Include Namespace="EventService"/><edmx:Include Namespace="EventService.v1_10_0"/></edmx:Reference>
  <edmx:DataServices>
    <Schema xmlns="http://docs.oasis-open.org/odata/ns/edm" Namespace="Service">
      <EntityContainer Name="Service" Extends="ServiceRoot.v1_15_0.ServiceContainer"/>
    </Schema>
  </edmx:DataServices>
</edmx:Edmx>
`
