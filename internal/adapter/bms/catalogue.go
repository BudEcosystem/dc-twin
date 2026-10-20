package bms

import "strings"

// The point catalogue: what DSX Exchange's BMS AsyncAPI 1.0.0 (schemas/asyncapi/bms/bms.yaml)
// requires of each object type and point type the twin publishes. A metadata payload is built
// from it, so a point the catalogue does not hold is never published with a guessed shape.

// state is one entry of a stateText array.
type state struct {
	Value int    `json:"value"`
	Text  string `json:"text"`
}

// pointDef is the AsyncAPI's contract for one objectType/pointType.
type pointDef struct {
	// engUnit, when set, is the one unit the schema allows (Rack points) or the typical unit
	// (equipment points); stateText is used instead for binary and enumerated points.
	engUnit   string
	stateText []state
	// integration: the value is published by an integration on BMS/v1/{integration}/Value/…;
	// the BMS publishes only the metadata, carrying the integration field.
	integration bool
	// deadband is the change below which an analog value is not republished before the
	// periodic republication (0: any change).
	deadband float64
}

var (
	leakStates     = []state{{0, "NoLeak"}, {1, "Leak"}}
	faultStates    = []state{{0, "NoFault"}, {1, "Fault"}}
	isolatedStates = []state{{0, "NotIsolated"}, {1, "Isolated"}}
	requestStates  = []state{{0, "NoIsolationRequested"}, {1, "IsolationRequested"}}
	// RackLiquidIsolationRequest also defines -1 (Unknown).
	liquidRequestStates = []state{{-1, "Unknown"}, {0, "NoIsolationRequested"}, {1, "IsolationRequested"}}
)

// catalogue is keyed "objectType/pointType".
var catalogue = map[string]pointDef{
	// Rack: BMS-published (RackBmsPointType). The Rack fields fix engUnit or require stateText.
	"Rack/RackLiquidSupplyTemperature":   {engUnit: "C", deadband: 0.1},
	"Rack/RackLiquidReturnTemperature":   {engUnit: "C", deadband: 0.1},
	"Rack/RackLiquidFlow":                {engUnit: "LPM", deadband: 0.5},
	"Rack/RackPower":                     {engUnit: "kW", deadband: 0.01},
	"Rack/RackLeakDetect":                {stateText: leakStates},
	"Rack/RackLeakSensorFault":           {stateText: faultStates},
	"Rack/RackLiquidIsolationStatus":     {stateText: isolatedStates},
	"Rack/RackElectricalIsolationStatus": {stateText: isolatedStates},
	// Rack: integration-published (RackIntegrationPointType).
	"Rack/RackLeakDetectTray":             {stateText: leakStates, integration: true},
	"Rack/RackLiquidIsolationRequest":     {stateText: liquidRequestStates, integration: true},
	"Rack/RackElectricalIsolationRequest": {stateText: requestStates, integration: true},

	// System: the BMS's own heartbeat (value = epoch milliseconds), and the integration's, whose
	// metadata the BMS publishes (objectName and objectId name the integration) and whose value
	// the integration publishes.
	"System/HeartbeatTimestampBms":         {},
	"System/HeartbeatTimestampIntegration": {integration: true},
}

// equipmentPoints are the BMS-published equipment point types the twin can publish for any
// EquipmentObjectType (CDU, Pump, Valve, Sensor …), in named-object mode.
var equipmentPoints = map[string]pointDef{
	"LiquidTemperature":          {engUnit: "C", deadband: 0.1},
	"LiquidFlow":                 {engUnit: "LPM", deadband: 0.5},
	"LiquidPressure":             {engUnit: "kPa", deadband: 0.5},
	"LiquidDifferentialPressure": {engUnit: "kPa", deadband: 0.5},
	"AirTemperature":             {engUnit: "C", deadband: 0.1},
	"PumpSpeed":                  {engUnit: "%", deadband: 0.5},
	"ValvePosition":              {engUnit: "%", deadband: 0.5},
	"LeakDetect":                 {stateText: leakStates},
}

// equipmentObjectTypes is the AsyncAPI's EquipmentObjectType enum.
var equipmentObjectTypes = map[string]bool{
	"CDU": true, "CoolingTower": true, "HX": true, "CRAH": true, "CRAC": true, "AHU": true, "Chiller": true,
	"BESS": true, "UPS": true, "ATS": true, "Generator": true, "Shunt": true, "Breaker": true, "Valve": true,
	"Pump": true, "Fan": true, "Damper": true, "Sensor": true, "Tank": true, "GenericObject": true,
}

// alias translates a profile's bms pointType that the AsyncAPI does not define into the one it
// does, with the processArea that tells points of one type on one object apart (the
// integration guide's convention: Secondary/Supply, Secondary/Return for the rack-facing loop).
type alias struct {
	pointType   string
	processArea []string
}

var aliases = map[string]alias{
	"SupplyTemperature": {"LiquidTemperature", []string{"Secondary", "Supply"}},
	"ReturnTemperature": {"LiquidTemperature", []string{"Secondary", "Return"}},
	"Flow":              {"LiquidFlow", []string{"Secondary"}},
}

// lookup resolves a profile's (objectType, pointType) to the AsyncAPI's, with its contract.
func lookup(objectType, pointType string) (pt string, area []string, def pointDef, ok bool) {
	pt = pointType
	if equipmentObjectTypes[objectType] {
		if a, isAlias := aliases[pointType]; isAlias {
			pt, area = a.pointType, a.processArea
		}
		def, ok = equipmentPoints[pt]
		return pt, area, def, ok
	}
	def, ok = catalogue[objectType+"/"+pt]
	return pt, nil, def, ok
}

// metadataSchema names the AsyncAPI component schema a metadata payload conforms to.
func metadataSchema(objectType, pointType string) string {
	if objectType == "Rack" {
		return pointType + "Metadata"
	}
	return objectType + pointType + "Metadata"
}

// unitScale converts a twin sensor unit to the AsyncAPI engUnit.
func unitScale(from, to string) float64 {
	switch {
	case strings.EqualFold(from, "W") && to == "kW":
		return 0.001
	case strings.EqualFold(from, "kW") && to == "W":
		return 1000
	}
	return 1 // C→C, L/min→LPM, %→%
}

// legacyPointType is the point type NICo's dsx-exchange-consumer recognises (its messages.rs,
// written before AsyncAPI 1.0.0) for a BMS-published Rack leak point.
var legacyPointType = map[string]string{
	"RackLeakDetect":      "LeakDetectRack",
	"RackLeakSensorFault": "LeakSensorFaultRack",
}
