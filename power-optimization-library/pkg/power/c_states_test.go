package power

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func setupCpuCStatesTests(cpufiles map[string]map[string]map[string]string) func() {
	origBasePath := basePath
	basePath = "testing/cpus"

	origGetNumOfCpusFunc := getNumberOfCpus
	getNumberOfCpus = func() uint {
		if _, ok := cpufiles["Driver"]; ok {
			return uint(len(cpufiles) - 1)
		} else {
			return uint(len(cpufiles))
		}
	}

	featureList[CStatesFeature].err = nil
	for cpu, states := range cpufiles {
		if cpu == "Driver" {
			err := os.MkdirAll(filepath.Join(basePath, strings.Split(cStatesDrvPath, "/")[0]), os.ModePerm)
			if err != nil {
				panic(err)
			}
			for driver := range states {
				err := os.WriteFile(filepath.Join(basePath, cStatesDrvPath), []byte(driver), 0644)
				if err != nil {
					panic(err)
				}
				break
			}
			continue
		}
		cpuStatesDir := filepath.Join(basePath, cpu, cStatesDir)
		err := os.MkdirAll(filepath.Join(cpuStatesDir), os.ModePerm)
		if err != nil {
			panic(err)
		}
		for state, props := range states {
			err := os.Mkdir(filepath.Join(cpuStatesDir, state), os.ModePerm)
			if err != nil {
				//panic(err)
			}
			for propFile, value := range props {
				err := os.WriteFile(filepath.Join(cpuStatesDir, state, propFile), []byte(value), 0644)
				if err != nil {
					panic(err)
				}
			}
		}
	}

	return func() {
		err := os.RemoveAll(strings.Split(basePath, "/")[0])
		if err != nil {
			panic(err)
		}
		basePath = origBasePath
		getNumberOfCpus = origGetNumOfCpusFunc
		cStatesNamesMap = map[string]int{}
		featureList[CStatesFeature].err = uninitialisedErr
	}
}

func Test_mapAvailableCStates(t *testing.T) {
	states := map[string]map[string]string{
		"state0":   {"name": "C0"},
		"state1":   {"name": "C1"},
		"state2":   {"name": "C2"},
		"state3":   {"name": "POLL"},
		"notState": nil,
	}
	cpufiles := map[string]map[string]map[string]string{
		"cpu0": states,
		"cpu1": states,
	}
	teardown := setupCpuCStatesTests(cpufiles)

	err := mapAvailableCStates()
	assert.NoError(t, err)

	assert.Equal(t, cStatesNamesMap, map[string]int{
		"C0":   0,
		"C1":   1,
		"C2":   2,
		"POLL": 3,
	})

	teardown()

	states["state0"] = nil
	teardown = setupCpuCStatesTests(cpufiles)

	err = mapAvailableCStates()

	assert.Error(t, err)

	teardown()

	states["state0"] = map[string]string{"name": "C0"}
	delete(cpufiles, "cpu0")
	teardown = setupCpuCStatesTests(cpufiles)

	assert.Error(t, mapAvailableCStates())
	teardown()
}

func TestCStates_preCheckCStates(t *testing.T) {
	teardown := setupCpuCStatesTests(map[string]map[string]map[string]string{
		"cpu0":   nil,
		"Driver": {"intel_idle\n": nil},
	})
	defer teardown()
	state := initCStates()
	assert.Equal(t, "C-States", state.name)
	assert.Equal(t, "intel_idle", state.driver)
	assert.Nil(t, state.FeatureError())
	teardown()

	teardown = setupCpuCStatesTests(map[string]map[string]map[string]string{
		"Driver": {"something": nil},
	})
	feature := initCStates()
	assert.ErrorContains(t, feature.FeatureError(), "unsupported")
	assert.Equal(t, "something", feature.driver)
	teardown()
}

func TestCpuImpl_applyCStates(t *testing.T) {
	states := map[string]map[string]string{
		"state0": {"name": "C0", "disable": "0"},
		"state2": {"name": "C2", "disable": "0"},
	}
	cpufiles := map[string]map[string]map[string]string{
		"cpu0": states,
	}
	defer setupCpuCStatesTests(cpufiles)()
	cStatesNamesMap = map[string]int{
		"C2": 2,
		"C0": 0,
	}
	err := (&cpuImpl{id: 0}).applyCStates(cstatesImpl{
		"C0": false,
		"C2": true})

	assert.NoError(t, err)

	stateFilePath := filepath.Join(
		basePath,
		fmt.Sprint("cpu", 0),
		fmt.Sprintf(cStateDisableFileFmt, 0),
	)
	disabled, _ := readStringFromFile(stateFilePath)
	assert.Equal(t, "1", disabled)

	stateFilePath = filepath.Join(
		basePath,
		fmt.Sprint("cpu", 0),
		fmt.Sprintf(cStateDisableFileFmt, 2),
	)
	disabled, _ = readStringFromFile(stateFilePath)
	assert.Equal(t, "0", disabled)
}

func TestValidateCStates(t *testing.T) {
	defer setupCpuCStatesTests(nil)()

	cStatesNamesMap = map[string]int{
		"C0": 0,
		"C2": 2,
		"C3": 3,
	}

	assert.NoError(t, ValidateCStates(map[string]bool{
		"C0": true,
		"C2": false,
	}))

	assert.ErrorContains(t, ValidateCStates(map[string]bool{
		"C9": false,
	}), "does not exist on this system")
}

func TestAvailableCStates(t *testing.T) {
	cStatesNamesMap = map[string]int{
		"C1": 1,
		"C2": 2,
		"C3": 3,
	}

	assert.ElementsMatch(t, GetAvailableCStates(), []string{"C1", "C2", "C3"})
}

func TestCpuImpl_updateCStates(t *testing.T) {
	core := &cpuImpl{id: 0}
	// cstates feature not supported
	assert.NoError(t, core.updateCStates())

	defer setupCpuCStatesTests(map[string]map[string]map[string]string{
		"cpu0": {
			"state0": {"name": "C0", "disable": "0"},
			"state1": {"name": "C1", "disable": "0"},
		},
	})()

	cStatesNamesMap["C0"] = 0
	cStatesNamesMap["C1"] = 1

	stateFilePath := filepath.Join(
		basePath,
		fmt.Sprint("cpu", 0),
		fmt.Sprintf(cStateDisableFileFmt, 0),
	)

	// read pool property
	pool := new(poolMock)
	profile := &profileImpl{cstates: cstatesImpl{"C0": true}}
	pool.On("GetPowerProfile").Return(profile)
	core.pool = pool
	assert.NoError(t, core.updateCStates())
	value, _ := os.ReadFile(stateFilePath)
	assert.Equal(t, "0", string(value), "expecting cstate to be enabled")
	pool.AssertExpectations(t)

	// no power profile, use default
	defaultCStates = cstatesImpl{"C0": false}
	pool = new(poolMock)
	pool.On("GetPowerProfile").Return(nil)
	core.pool = pool
	assert.NoError(t, core.updateCStates())
	value, _ = os.ReadFile(stateFilePath)
	assert.Equal(t, "1", string(value), "expecting cstate to be disabled")
	pool.AssertExpectations(t)
}
