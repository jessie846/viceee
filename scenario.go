// scenario.go
// Copyright(c) 2022 Matt Pharr, licensed under the GNU Public License, Version 3.
// SPDX: GPL-3.0-only

package main

import (
	"embed"
	"encoding/xml"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

type Scenario struct {
	Name              string                      `json:"name"`
	Airports          map[string]*Airport         `json:"airports"`
	VideoMapFile      string                      `json:"video_map_file"`
	Fixes             map[string]Point2LL         `json:"fixes"`
	ScenarioConfigs   map[string]*ScenarioConfig  `json:"configs"`
	DefaultController string                      `json:"default_controller"`
	DefaultScenario   string                      `json:"default_scenario"`
	ControlPositions  map[string]*Controller      `json:"control_positions"`
	Scratchpads       map[string]string           `json:"scratchpads"`
	AirspaceVolumes   map[string][]AirspaceVolume `json:"-"` // for now, parsed from the XML...
	ArrivalGroups     map[string][]Arrival        `json:"arrival_groups"`

	Center         Point2LL              `json:"center"`
	PrimaryAirport string                `json:"primary_airport"`
	RadarSites     map[string]*RadarSite `json:"radar_sites"`
	STARSMaps      []STARSMap            `json:"stars_maps"`

	NmPerLatitude     float32 `json:"nm_per_latitude"`
	NmPerLongitude    float32 `json:"nm_per_longitude"`
	MagneticVariation float32 `json:"magnetic_variation"`
}

type Arrival struct {
	Waypoints       WaypointArray            `json:"waypoints"`
	RunwayWaypoints map[string]WaypointArray `json:"runway_waypoints"`
	Route           string                   `json:"route"`

	InitialController string `json:"initial_controller"`
	InitialAltitude   int    `json:"initial_altitude"`
	ClearedAltitude   int    `json:"cleared_altitude"`
	InitialSpeed      int    `json:"initial_speed"`
	SpeedRestriction  int    `json:"speed_restriction"`
	ExpectApproach    string `json:"expect_approach"`
	Scratchpad        string `json:"scratchpad"`

	Airlines map[string][]ArrivalAirline `json:"airlines"`
}

type ArrivalAirline struct {
	ICAO    string `json:"icao"`
	Airport string `json:"airport"`
	Fleet   string `json:"fleet,omitempty"`
}

type AirspaceVolume struct {
	LowerLimit, UpperLimit int
	Boundaries             [][]Point2LL
}

type ScenarioConfig struct {
	Name        string   `json:"name"`
	Callsign    string   `json:"callsign"`
	Wind        Wind     `json:"wind"`
	Controllers []string `json:"controllers"`

	// Map from arrival group name to map from airport name to rate...
	ArrivalGroupRates map[string]map[string]*int32 `json:"arrivals"`

	// Key is arrival group name
	nextArrivalSpawn map[string]time.Time

	ApproachAirspace       []AirspaceVolume `json:"-"`
	DepartureAirspace      []AirspaceVolume `json:"-"`
	ApproachAirspaceNames  []string         `json:"approach_airspace"`
	DepartureAirspaceNames []string         `json:"departure_airspace"`

	DepartureRunways []ScenarioDepartureRunway `json:"departure_runways,omitempty"`
	ArrivalRunways   []ScenarioArrivalRunway   `json:"arrival_runways,omitempty"`

	// The same runway may be present multiple times in DepartureRunways,
	// with different Category values. However, we want to make sure that
	// we don't spawn two aircraft on the same runway at the same time (or
	// close to it).  Therefore, here we track a per-runway "when's the
	// next time that we will spawn *something* from the runway" time.
	// When the time is up, we'll figure out which specific matching entry
	// in DepartureRunways to use...
	nextDepartureSpawn map[string]time.Time
}

type ScenarioDepartureRunway struct {
	Airport  string `json:"airport"`
	Runway   string `json:"runway"`
	Category string `json:"category,omitempty"`
	Rate     int32  `json:"rate"`

	lastDeparture *Departure
	exitRoutes    map[string]ExitRoute // copied from DepartureRunway
}

type ScenarioArrivalRunway struct {
	Airport string `json:"airport"`
	Runway  string `json:"runway"`
}

type Wind struct {
	Direction int32 `json:"direction"`
	Speed     int32 `json:"speed"`
	Gust      int32 `json:"gust"`
}

func (s *ScenarioConfig) AllAirports() []string {
	return append(s.DepartureAirports(), s.ArrivalAirports()...)
}

func (s *ScenarioConfig) DepartureAirports() []string {
	m := make(map[string]interface{})
	for _, rwy := range s.DepartureRunways {
		m[rwy.Airport] = nil
	}
	return SortedMapKeys(m)
}

func (s *ScenarioConfig) ArrivalAirports() []string {
	m := make(map[string]interface{})
	for _, rwy := range s.ArrivalRunways {
		m[rwy.Airport] = nil
	}
	return SortedMapKeys(m)
}

func (s *ScenarioConfig) runwayDepartureRate(ar string) int {
	r := 0
	for _, rwy := range s.DepartureRunways {
		if ar == rwy.Airport+"/"+rwy.Runway {
			r += int(rwy.Rate)
		}
	}
	return r
}

func (s *ScenarioConfig) PostDeserialize(t *Scenario) []error {
	var errors []error

	for _, as := range s.ApproachAirspaceNames {
		if vol, ok := t.AirspaceVolumes[as]; !ok {
			errors = append(errors, fmt.Errorf("%s: unknown approach airspace in scenario %s", as, s.Name))
		} else {
			s.ApproachAirspace = append(s.ApproachAirspace, vol...)
		}
	}
	for _, as := range s.DepartureAirspaceNames {
		if vol, ok := t.AirspaceVolumes[as]; !ok {
			errors = append(errors, fmt.Errorf("%s: unknown departure airspace in scenario %s", as, s.Name))
		} else {
			s.DepartureAirspace = append(s.DepartureAirspace, vol...)
		}
	}

	sort.Slice(s.DepartureRunways, func(i, j int) bool {
		if s.DepartureRunways[i].Airport != s.DepartureRunways[j].Airport {
			return s.DepartureRunways[i].Airport < s.DepartureRunways[j].Airport
		} else if s.DepartureRunways[i].Runway != s.DepartureRunways[j].Runway {
			return s.DepartureRunways[i].Runway < s.DepartureRunways[j].Runway
		} else {
			return s.DepartureRunways[i].Category < s.DepartureRunways[j].Category
		}
	})

	s.nextDepartureSpawn = make(map[string]time.Time)
	for i, rwy := range s.DepartureRunways {
		if ap, ok := t.Airports[rwy.Airport]; !ok {
			errors = append(errors, fmt.Errorf("%s: airport not found for departure runway in scenario %s", rwy.Airport, s.Name))
		} else {
			idx := FindIf(ap.DepartureRunways, func(r *DepartureRunway) bool { return r.Runway == rwy.Runway })
			if idx == -1 {
				errors = append(errors, fmt.Errorf("%s: runway not found at airport %s for departure runway in scenario %s",
					rwy.Runway, rwy.Airport, s.Name))
			} else {
				s.DepartureRunways[i].exitRoutes = ap.DepartureRunways[idx].ExitRoutes
			}
			s.nextDepartureSpawn[rwy.Airport+"/"+rwy.Runway] = time.Time{}

			if rwy.Category != "" {
				found := false
				for _, dep := range ap.Departures {
					if ap.ExitCategories[dep.Exit] == rwy.Category {
						found = true
						break
					}
				}
				if !found {
					errors = append(errors,
						fmt.Errorf("%s: no departures from %s have exit category specified for departure runway %s in scenario %s",
							rwy.Category, rwy.Airport, rwy.Runway, s.Name))
				}
			}
		}
	}

	sort.Slice(s.ArrivalRunways, func(i, j int) bool {
		if s.ArrivalRunways[i].Airport == s.ArrivalRunways[j].Airport {
			return s.ArrivalRunways[i].Runway < s.ArrivalRunways[j].Runway
		}
		return s.ArrivalRunways[i].Airport < s.ArrivalRunways[j].Airport
	})

	for _, rwy := range s.ArrivalRunways {
		if ap, ok := t.Airports[rwy.Airport]; !ok {
			errors = append(errors, fmt.Errorf("%s: airport not found for arrival runway in scenario %s", rwy.Airport, s.Name))
		} else if FindIf(ap.ArrivalRunways, func(r *ArrivalRunway) bool { return r.Runway == rwy.Runway }) == -1 {
			errors = append(errors, fmt.Errorf("%s: runway not found for arrival runway at airport %s in scenario %s",
				rwy.Runway, rwy.Airport, s.Name))
		}
	}

	s.nextArrivalSpawn = make(map[string]time.Time)

	for _, name := range SortedMapKeys(s.ArrivalGroupRates) {
		// Make sure the arrival group has been defined
		if arrivals, ok := t.ArrivalGroups[name]; !ok {
			errors = append(errors, fmt.Errorf("%s: arrival group not found in TRACON in scenario %s", name, s.Name))
		} else {
			// Check the airports in it
			for airport := range s.ArrivalGroupRates[name] {
				if _, ok := t.Airports[airport]; !ok {
					errors = append(errors, fmt.Errorf("%s: unknown arrival airport in %s arrival group in scenario %s",
						airport, name, s.Name))
				} else {
					found := false
					for _, ar := range arrivals {
						if _, ok := ar.Airlines[airport]; ok {
							found = true
							break
						}
					}
					if !found {
						errors = append(errors, fmt.Errorf("%s: airport not included in any arrivals in %s arrival group in scenario %s",
							airport, name, s.Name))
					}
				}
			}
		}
	}

	for _, ctrl := range s.Controllers {
		if _, ok := t.ControlPositions[ctrl]; !ok {
			errors = append(errors, fmt.Errorf("%s: controller unknown in scenario %s", ctrl, s.Name))
		}
	}

	return errors
}

///////////////////////////////////////////////////////////////////////////
// Scenario

func (t *Scenario) Locate(s string) (Point2LL, bool) {
	// Scenario's definitions take precedence...
	if ap, ok := t.Airports[s]; ok {
		return ap.Location, true
	} else if p, ok := t.Fixes[s]; ok {
		return p, true
	} else if n, ok := database.Navaids[strings.ToUpper(s)]; ok {
		return n.Location, ok
	} else if f, ok := database.Fixes[strings.ToUpper(s)]; ok {
		return f.Location, ok
	} else if p, err := ParseLatLong(s); err == nil {
		return p, true
	} else {
		return Point2LL{}, false
	}
}

func (t *Scenario) PostDeserialize() {
	t.AirspaceVolumes = parseAirspace()

	var errors []error
	for name, ap := range t.Airports {
		if name != ap.ICAO {
			errors = append(errors, fmt.Errorf("%s: airport Name doesn't match (%s)", name, ap.ICAO))
		}
		for _, err := range ap.PostDeserialize(t) {
			errors = append(errors, fmt.Errorf("%s: error in specification: %v", ap.ICAO, err))
		}
	}

	if _, ok := t.ScenarioConfigs[t.DefaultScenario]; !ok {
		errors = append(errors, fmt.Errorf("%s: default scenario not found in %s", t.DefaultScenario, t.Name))
	}

	if _, ok := t.ControlPositions[t.DefaultController]; !ok {
		errors = append(errors, fmt.Errorf("%s: default controller not found in %s", t.DefaultController, t.Name))
	} else {
		// make sure the controller has at least one scenario..
		found := false
		for _, sc := range t.ScenarioConfigs {
			if sc.Callsign == t.DefaultController {
				found = true
				break
			}
		}
		if !found {
			errors = append(errors, fmt.Errorf("%s: default controller not used in any scenarios in %s",
				t.DefaultController, t.Name))
		}
	}

	if len(t.RadarSites) == 0 {
		errors = append(errors, fmt.Errorf("No radar sites specified in tracon %s", t.Name))
	}
	for name, rs := range t.RadarSites {
		if _, ok := t.Locate(rs.Position); rs.Position == "" || !ok {
			errors = append(errors, fmt.Errorf("%s: radar site position not found in %s", name, t.Name))
		} else if rs.Char == "" {
			errors = append(errors, fmt.Errorf("%s: radar site missing character id in %s", name, t.Name))
		}
	}

	for name, arrivals := range t.ArrivalGroups {
		if len(arrivals) == 0 {
			errors = append(errors, fmt.Errorf("%s: no arrivals in arrival group in %s", name, t.Name))
		}

		for _, ar := range arrivals {
			for _, err := range t.InitializeWaypointLocations(ar.Waypoints) {
				errors = append(errors, fmt.Errorf("%s: %v in %s", name, err, t.Name))
			}
			for _, wp := range ar.RunwayWaypoints {
				for _, err := range t.InitializeWaypointLocations(wp) {
					errors = append(errors, fmt.Errorf("%s: %v in %s", name, err, t.Name))
				}
			}

			for _, apAirlines := range ar.Airlines {
				for _, al := range apAirlines {
					for _, err := range database.CheckAirline(al.ICAO, al.Fleet) {
						errors = append(errors, fmt.Errorf("%v in %s", err, t.Name))
					}
				}
			}

			if _, ok := t.ControlPositions[ar.InitialController]; !ok {
				errors = append(errors, fmt.Errorf("%s: controller not found for arrival in %s group in %s",
					ar.InitialController, name, t.Name))
			}
		}
	}

	// Do after airports!
	for _, s := range t.ScenarioConfigs {
		errors = append(errors, s.PostDeserialize(t)...)
	}

	if len(errors) > 0 {
		for _, err := range errors {
			lg.Errorf("%v", err)
		}
		os.Exit(1)
	}
}

func (t *Scenario) InitializeWaypointLocations(waypoints []Waypoint) []error {
	var prev Point2LL
	var errors []error

	for i, wp := range waypoints {
		if pos, ok := t.Locate(wp.Fix); ok {
			waypoints[i].Location = pos
		} else {
			errors = append(errors, fmt.Errorf("%s: unable to locate waypoint", wp.Fix))
			continue
		}

		d := nmdistance2ll(prev, waypoints[i].Location)
		if i > 1 && d > 50 {
			errors = append(errors, fmt.Errorf("%s: waypoint at %s is suspiciously far from previous one (%s at %s): %f nm",
				wp.Fix, waypoints[i].Location.DDString(), waypoints[i-1].Fix, waypoints[i-1].Location.DDString(), d))
		}
		prev = waypoints[i].Location
	}
	return errors
}

///////////////////////////////////////////////////////////////////////////
// Airspace

//go:embed resources/ZNY_sanscomment_VOLUMES.xml
var znyVolumesXML string

type XMLBoundary struct {
	Name     string `xml:"Name,attr"`
	Segments string `xml:",chardata"`
}

type XMLVolume struct {
	Name       string `xml:"Name,attr"`
	LowerLimit int    `xml:"LowerLimit,attr"`
	UpperLimit int    `xml:"UpperLimit,attr"`
	Boundaries string `xml:"Boundaries"`
}

type XMLAirspace struct {
	XMLName    xml.Name      `xml:"Volumes"`
	Boundaries []XMLBoundary `xml:"Boundary"`
	Volumes    []XMLVolume   `xml:"Volume"`
}

func parseAirspace() map[string][]AirspaceVolume {
	var xair XMLAirspace
	if err := xml.Unmarshal([]byte(znyVolumesXML), &xair); err != nil {
		panic(err)
	}

	//lg.Errorf("%s", spew.Sdump(vol))

	boundaries := make(map[string][]Point2LL)
	volumes := make(map[string][]AirspaceVolume)

	for _, b := range xair.Boundaries {
		var pts []Point2LL
		for _, ll := range strings.Split(b.Segments, "/") {
			p, err := ParseLatLong(strings.TrimSpace(ll))
			if err != nil {
				lg.Errorf("%s: %v", ll, err)
			} else {
				pts = append(pts, p)
			}
		}
		if _, ok := boundaries[b.Name]; ok {
			lg.Errorf("%s: boundary redefined", b.Name)
		}
		boundaries[b.Name] = pts
	}

	for _, v := range xair.Volumes {
		vol := AirspaceVolume{
			LowerLimit: v.LowerLimit,
			UpperLimit: v.UpperLimit,
		}

		for _, name := range strings.Split(v.Boundaries, ",") {
			if b, ok := boundaries[name]; !ok {
				lg.Errorf("%s: boundary in volume %s has not been defined. Volume may be invalid",
					name, v.Name)
			} else {
				vol.Boundaries = append(vol.Boundaries, b)
			}
		}

		volumes[v.Name] = append(volumes[v.Name], vol)
	}

	return volumes
}

func InAirspace(p Point2LL, alt float32, volumes []AirspaceVolume) (bool, [][2]int) {
	var altRanges [][2]int
	for _, v := range volumes {
		inside := false
		for _, pts := range v.Boundaries {
			if PointInPolygon(p, pts) {
				inside = !inside
			}
		}
		if inside {
			altRanges = append(altRanges, [2]int{v.LowerLimit, v.UpperLimit})
		}
	}

	// Sort altitude ranges and then merge ones that have 1000 foot separation
	sort.Slice(altRanges, func(i, j int) bool { return altRanges[i][0] < altRanges[j][0] })
	var mergedAlts [][2]int
	i := 0
	inside := false
	for i < len(altRanges) {
		low := altRanges[i][0]
		high := altRanges[i][1]

		for i+1 < len(altRanges) {
			if altRanges[i+1][0]-high <= 1000 {
				// merge
				high = altRanges[i+1][1]
				i++
			} else {
				break
			}
		}

		// 10 feet of slop for rounding error
		inside = inside || (int(alt)+10 >= low && int(alt)-10 <= high)

		mergedAlts = append(mergedAlts, [2]int{low, high})
		i++
	}

	return inside, mergedAlts
}

///////////////////////////////////////////////////////////////////////////
// LoadScenarios

var (
	//go:embed configs/*.json configs/*.json.zst
	embeddedJSON embed.FS
)

func LoadScenarios() map[string]*Scenario {
	videoMapCommandBuffers := make(map[string]map[string]CommandBuffer)
	scenarios := make(map[string]*Scenario)

	err := fs.WalkDir(embeddedJSON, "configs", func(path string, d fs.DirEntry, err error) error {
		lg.Printf("Loading embedded file %s", path)
		if d.IsDir() {
			return nil
		}
		contents, err := fs.ReadFile(embeddedJSON, path)
		if err != nil {
			return err
		}

		p := strings.ToLower(path)
		if strings.HasSuffix(p, ".zst") {
			contents = []byte(decompressZstd(string(contents)))
			p = p[:len(p)-4]
		}

		if !strings.HasSuffix(p, ".json") {
			return fmt.Errorf("%s: skipping file without .json extension", path)
		}

		if strings.HasSuffix(p, "-maps.json") {
			var maps map[string][]Point2LL
			if err := UnmarshalJSON(contents, &maps); err != nil {
				return err
			}

			vm := make(map[string]CommandBuffer)
			for name, segs := range maps {
				if _, ok := vm[name]; ok {
					return fmt.Errorf("%s: video map repeatedly defined in file %s", name, path)
				}

				ld := GetLinesDrawBuilder()
				for i := 0; i < len(segs)/2; i++ {
					ld.AddLine(segs[2*i], segs[2*i+1])
				}
				var cb CommandBuffer
				ld.GenerateCommands(&cb)

				vm[name] = cb

				ReturnLinesDrawBuilder(ld)
			}

			videoMapCommandBuffers[path] = vm
			return nil
		} else {
			var s Scenario
			if err := UnmarshalJSON(contents, &s); err != nil {
				return err
			}
			if s.Name == "" {
				return fmt.Errorf("%s: scenario definition is missing a \"name\" member", path)
			}
			if _, ok := scenarios[s.Name]; ok {
				return fmt.Errorf("%s: scenario repeatedly defined", s.Name)
			}

			scenarios[s.Name] = &s
			return nil
		}
	})

	if err != nil {
		lg.Errorf("%v", err)
		os.Exit(1)
	}

	for _, s := range scenarios {
		if s.VideoMapFile == "" {
			lg.Errorf("%s: scenario does not have \"video_map_file\" specified", s.Name)
			os.Exit(1)
		}
		if bufferMap, ok := videoMapCommandBuffers[s.VideoMapFile]; !ok {
			lg.Errorf("%s: \"video_map_file\" not found for scenario %s", s.VideoMapFile, s.Name)
		} else {
			for i, sm := range s.STARSMaps {
				if cb, ok := bufferMap[sm.Name]; !ok {
					lg.Errorf("%s: video map not found for scenario %s", sm.Name, s.Name)
					os.Exit(1)
				} else {
					s.STARSMaps[i].cb = cb
				}
			}
		}

		// Horribly hacky but PostDeserialize ends up calling functions
		// that access the scenario global (e.g. nmdistance2ll)...
		scenario = s
		s.PostDeserialize()
		scenario = nil
	}

	return scenarios
}