package power

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestNewProfile(t *testing.T) {
	oldgovs := availableGovs
	availableGovs = []string{cpuPolicyPowersave, cpuPolicyPerformance}
	cStatesNamesMap = map[string]int{"POLL": 0, "C1": 1, "C1E": 3, "C6": 2}

	profile, err := NewPowerProfile("name", 0, 100, cpuPolicyPowersave, "epp", map[string]bool{})
	assert.ErrorIs(t, err, uninitialisedErr)
	assert.Nil(t, profile)

	featureList[FrequencyScalingFeature].err = nil
	featureList[EPPFeature].err = nil
	featureList[CStatesFeature].err = nil
	defer func() { featureList[FrequencyScalingFeature].err = uninitialisedErr }()
	defer func() { featureList[EPPFeature].err = uninitialisedErr }()
	defer func() { featureList[CStatesFeature].err = uninitialisedErr }()
	defer func() { availableGovs = oldgovs }()

	profile, err = NewPowerProfile("name", 0, 100, cpuPolicyPowersave, "epp", map[string]bool{"C1": true, "C6": false})
	assert.NoError(t, err)
	assert.Equal(t, "name", profile.Name())
	assert.Equal(t, uint(0), profile.GetPStates().MinFreq())
	assert.Equal(t, uint(100*1000), profile.GetPStates().MaxFreq())
	assert.Equal(t, "powersave", profile.GetPStates().Governor())
	assert.Equal(t, "epp", profile.GetPStates().Epp())
	assert.Equal(t, map[string]bool{"C1": true, "C6": false}, profile.GetCStates().States())

	profile, err = NewPowerProfile("name", 0, 10, cpuPolicyPerformance, cpuPolicyPerformance, map[string]bool{"C1": false, "C6": false})
	assert.NoError(t, err)
	assert.Equal(t, "name", profile.Name())
	assert.Equal(t, uint(0), profile.GetPStates().MinFreq())
	assert.Equal(t, uint(10*1000), profile.GetPStates().MaxFreq())
	assert.Equal(t, "performance", profile.GetPStates().Governor())
	assert.Equal(t, "performance", profile.GetPStates().Epp())
	assert.Equal(t, map[string]bool{"C1": false, "C6": false}, profile.GetCStates().States())

	profile, err = NewPowerProfile("name", 0, 100, cpuPolicyPerformance, "epp", map[string]bool{})
	assert.ErrorContains(t, err, fmt.Sprintf("'%s' epp can be used with '%s' governor", cpuPolicyPerformance, cpuPolicyPerformance))
	assert.Nil(t, profile)

	profile, err = NewPowerProfile("name", 100, 0, cpuPolicyPowersave, "epp", map[string]bool{})
	assert.ErrorContains(t, err, "max frequency (0) cannot be lower than the min frequency (100)")
	assert.Nil(t, profile)

	profile, err = NewPowerProfile("name", 0, 100, "something random", "epp", map[string]bool{})
	assert.ErrorContains(t, err, "governor something random is not supported, please use one of the following")
	assert.Nil(t, profile)

	profile, err = NewPowerProfile("name", 0, 100, cpuPolicyPowersave, "epp", map[string]bool{"C7": true})
	assert.ErrorContains(t, err, "c-state C7 does not exist on this system")
	assert.Nil(t, profile)
}

func TestEfficientProfile(t *testing.T) {
	oldGovs := availableGovs
	availableGovs = []string{cpuPolicyPowersave, cpuPolicyPerformance}
	featureList[FrequencyScalingFeature].err = nil
	featureList[EPPFeature].err = nil
	featureList[CStatesFeature].err = nil
	typeCopy := coreTypes

	//reset values afterwards
	defer func() { featureList[FrequencyScalingFeature].err = uninitialisedErr }()
	defer func() { featureList[EPPFeature].err = uninitialisedErr }()
	defer func() { featureList[CStatesFeature].err = uninitialisedErr }()
	defer func() { coreTypes = typeCopy }()
	defer func() { availableGovs = oldGovs }()

	coreTypes = CoreTypeList{&CpuFrequencySet{min: 300, max: 1000}, &CpuFrequencySet{min: 300, max: 500}}

	//default scenario
	profile, err := NewEcorePowerProfile("name", 300, 1000, 300, 450, cpuPolicyPerformance, cpuPolicyPerformance, map[string]bool{})
	assert.NoError(t, err)
	assert.NotNil(t, profile)

	// invalid frequency ranges
	profile, err = NewEcorePowerProfile("name", 300, 1000, 430, 200, cpuPolicyPerformance, cpuPolicyPerformance, map[string]bool{})
	assert.ErrorContains(t, err, "max frequency (200) cannot be lower than the min frequency (430)")
	assert.Nil(t, profile)
}
