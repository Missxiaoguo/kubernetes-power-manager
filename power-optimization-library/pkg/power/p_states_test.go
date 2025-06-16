package power

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func setupCpuScalingTests(cpufiles map[string]map[string]string) func() {
	origBasePath := basePath
	basePath = "testing/cpus"
	defaultDefaultPStates := defaultPStates
	typeCopy := coreTypes
	referenceCopy := CpuTypeReferences
	// backup pointer to function that gets all CPUs
	// replace it with our controlled function
	origGetNumOfCpusFunc := getNumberOfCpus
	getNumberOfCpus = func() uint { return uint(len(cpufiles)) }

	// "initialise" P-States feature
	featureList[FrequencyScalingFeature].err = nil

	// if cpu0 is here we set its values to temporary defaultPStates
	if cpu0, ok := cpufiles["cpu0"]; ok {
		defaultPStates = &pstatesImpl{}
		if max, ok := cpu0["max"]; ok {
			max, _ := strconv.Atoi(max)
			defaultPStates.max = uint(max)
		}
		if min, ok := cpu0["min"]; ok {
			min, _ := strconv.Atoi(min)
			defaultPStates.min = uint(min)
		}
		if governor, ok := cpu0["governor"]; ok {
			defaultPStates.governor = governor
		}
		if epp, ok := cpu0["epp"]; ok {
			defaultPStates.epp = epp
		}
	}
	for cpuName, cpuDetails := range cpufiles {
		cpudir := filepath.Join(basePath, cpuName)
		os.MkdirAll(filepath.Join(cpudir, "cpufreq"), os.ModePerm)
		os.MkdirAll(filepath.Join(cpudir, "topology"), os.ModePerm)
		for prop, value := range cpuDetails {
			switch prop {
			case "driver":
				os.WriteFile(filepath.Join(cpudir, pStatesDrvFile), []byte(value+"\n"), 0664)
			case "max":
				os.WriteFile(filepath.Join(cpudir, scalingMaxFile), []byte(value+"\n"), 0644)
				os.WriteFile(filepath.Join(cpudir, cpuMaxFreqFile), []byte(value+"\n"), 0644)
			case "min":
				os.WriteFile(filepath.Join(cpudir, scalingMinFile), []byte(value+"\n"), 0644)
				os.WriteFile(filepath.Join(cpudir, cpuMinFreqFile), []byte(value+"\n"), 0644)
			case "package":
				os.WriteFile(filepath.Join(cpudir, packageIdFile), []byte(value+"\n"), 0644)
			case "die":
				os.WriteFile(filepath.Join(cpudir, dieIdFile), []byte(value+"\n"), 0644)
				os.WriteFile(filepath.Join(cpudir, coreIdFile), []byte(cpuName[3:]+"\n"), 0644)
			case "epp":
				os.WriteFile(filepath.Join(cpudir, eppFile), []byte(value+"\n"), 0644)
			case "governor":
				os.WriteFile(filepath.Join(cpudir, scalingGovFile), []byte(value+"\n"), 0644)
			case "available_governors":
				os.WriteFile(filepath.Join(cpudir, availGovFile), []byte(value+"\n"), 0644)
			}
		}
	}
	return func() {
		// wipe created cpus dir
		os.RemoveAll(strings.Split(basePath, "/")[0])
		// revert cpu /sys path
		basePath = origBasePath
		// revert get number of system cpus function
		getNumberOfCpus = origGetNumOfCpusFunc
		// revert scaling driver feature to un initialised state
		featureList[FrequencyScalingFeature].err = uninitialisedErr
		coreTypes = typeCopy
		CpuTypeReferences = referenceCopy
		// revert default pstates
		defaultPStates = defaultDefaultPStates
	}
}

func TestIsScalingDriverSupported(t *testing.T) {
	assert.False(t, isScalingDriverSupported("something"))
	assert.True(t, isScalingDriverSupported("intel_pstate"))
	assert.True(t, isScalingDriverSupported("intel_cpufreq"))
	assert.True(t, isScalingDriverSupported("acpi-cpufreq"))
}
func TestPreChecksScalingDriver(t *testing.T) {
	var pStates featureStatus
	origpath := basePath
	basePath = ""
	pStates = initScalingDriver()

	assert.Equal(t, pStates.name, "Frequency-Scaling")
	assert.ErrorContains(t, pStates.err, "failed to determine driver")
	epp := initEpp()
	assert.Equal(t, epp.name, "Energy-Performance-Preference")
	assert.ErrorContains(t, epp.err, "EPP file cpufreq/energy_performance_preference does not exist")
	basePath = origpath
	teardown := setupCpuScalingTests(map[string]map[string]string{
		"cpu0": {
			"min":                 "111",
			"max":                 "999",
			"driver":              "intel_pstate",
			"available_governors": "performance",
			"epp":                 "performance",
		},
	})

	pStates = initScalingDriver()
	assert.Equal(t, "intel_pstate", pStates.driver)
	assert.NoError(t, pStates.err)
	epp = initEpp()
	assert.NoError(t, epp.err)

	teardown()
	defer setupCpuScalingTests(map[string]map[string]string{
		"cpu0": {
			"driver": "some_unsupported_driver",
		},
	})()

	pStates = initScalingDriver()
	assert.ErrorContains(t, pStates.err, "unsupported")
	assert.Equal(t, pStates.driver, "some_unsupported_driver")
	teardown()
	defer setupCpuScalingTests(map[string]map[string]string{
		"cpu0": {
			"driver":              "acpi-cpufreq",
			"available_governors": "powersave",
			"max":                 "3700",
			"min":                 "3200",
		},
	})()
	acpi := initScalingDriver()
	assert.Equal(t, "acpi-cpufreq", acpi.driver)
	assert.NoError(t, acpi.err)
}

func TestCoreImpl_updateFreqValues(t *testing.T) {
	var core *cpuImpl
	const (
		maxDefault   = 9990
		maxFreqToSet = 8888
		minFreqToSet = 1000
	)
	typeCopy := coreTypes
	coreTypes = CoreTypeList{&CpuFrequencySet{min: minFreqToSet, max: maxDefault}}
	defer func() { coreTypes = typeCopy }()

	core = &cpuImpl{}
	// p-states not supported
	assert.NoError(t, core.updateFrequencies())

	teardown := setupCpuScalingTests(map[string]map[string]string{
		"cpu0": {
			"max": fmt.Sprint(maxDefault),
			"min": fmt.Sprint(minFreqToSet),
		},
	})

	defer teardown()

	// set desired power profile
	host := new(hostMock)
	pool := new(poolMock)
	core = &cpuImpl{
		id:   0,
		pool: pool,
		core: &cpuCore{coreType: 0},
	}
	profile := &profileImpl{pstates: &pstatesImpl{max: maxFreqToSet, min: minFreqToSet}, cstates: cstatesImpl{}}
	pool.On("GetPowerProfile").Return(profile)
	pool.On("getHost").Return(host)
	host.On("NumCoreTypes").Return(uint(1))

	assert.NoError(t, core.updateFrequencies())
	maxFreqContent, _ := os.ReadFile(filepath.Join(basePath, "cpu0", scalingMaxFile))
	maxFreqInt, _ := strconv.Atoi(string(maxFreqContent))
	assert.Equal(t, maxFreqToSet, maxFreqInt)
	pool.AssertNumberOfCalls(t, "GetPowerProfile", 1)

	// set default power profile
	pool = new(poolMock)
	core.pool = pool
	pool.On("GetPowerProfile").Return(nil)
	pool.On("getHost").Return(host)
	assert.NoError(t, core.updateFrequencies())
	maxFreqContent, _ = os.ReadFile(filepath.Join(basePath, "cpu0", scalingMaxFile))
	maxFreqInt, _ = strconv.Atoi(string(maxFreqContent))
	assert.Equal(t, maxDefault, maxFreqInt)
	pool.AssertNumberOfCalls(t, "GetPowerProfile", 1)

}

func TestCoreImpl_setPstatsValues(t *testing.T) {
	const (
		maxFreqToSet  = 8888
		minFreqToSet  = 1111
		governorToSet = "powersave"
		eppToSet      = "testEpp"
	)
	featureList[FrequencyScalingFeature].err = nil
	featureList[EPPFeature].err = nil
	typeCopy := coreTypes
	coreTypes = CoreTypeList{&CpuFrequencySet{min: 1000, max: 9000}}
	defer func() { coreTypes = typeCopy }()
	defer func() { featureList[EPPFeature].err = uninitialisedErr }()
	defer func() { featureList[FrequencyScalingFeature].err = uninitialisedErr }()

	poolmk := new(poolMock)
	host := new(hostMock)
	poolmk.On("getHost").Return(host)
	host.On("NumCoreTypes").Return(uint(1))
	core := &cpuImpl{
		id:   0,
		core: &cpuCore{id: 0, coreType: 0},
		pool: poolmk,
	}

	teardown := setupCpuScalingTests(map[string]map[string]string{
		"cpu0": {
			"governor": "performance",
			"max":      "9999",
			"min":      "999",
			"epp":      "balance-performance",
		},
	})
	defer teardown()

	pstatesConfig := &pstatesImpl{
		max:      maxFreqToSet,
		min:      minFreqToSet,
		epp:      eppToSet,
		governor: governorToSet,
	}
	assert.NoError(t, core.setDriverValues(pstatesConfig))

	governorFileContent, _ := os.ReadFile(filepath.Join(basePath, "cpu0", scalingGovFile))
	assert.Equal(t, governorToSet, string(governorFileContent))

	eppFileContent, _ := os.ReadFile(filepath.Join(basePath, "cpu0", eppFile))
	assert.Equal(t, eppToSet, string(eppFileContent))

	maxFreqContent, _ := os.ReadFile(filepath.Join(basePath, "cpu0", scalingMaxFile))
	maxFreqInt, _ := strconv.Atoi(string(maxFreqContent))
	assert.Equal(t, maxFreqToSet, maxFreqInt)

	minFreqContent, _ := os.ReadFile(filepath.Join(basePath, "cpu0", scalingMaxFile))
	minFreqInt, _ := strconv.Atoi(string(minFreqContent))
	assert.Equal(t, maxFreqToSet, minFreqInt)

	// check for empty epp unset
	pstatesConfig.epp = ""
	assert.NoError(t, core.setDriverValues(pstatesConfig))
	eppFileContent, _ = os.ReadFile(filepath.Join(basePath, "cpu0", eppFile))
	assert.Equal(t, eppToSet, string(eppFileContent))
}
