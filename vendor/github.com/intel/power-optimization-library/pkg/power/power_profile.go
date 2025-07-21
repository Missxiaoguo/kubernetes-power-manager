package power

import (
	"fmt"
	"slices"
)

type profileImpl struct {
	name    string
	pstates PStates
	cstates CStates
}

// Profile contains both P-states and C-states information
type Profile interface {
	Name() string
	GetPStates() PStates
	GetCStates() CStates
}

func (p *profileImpl) Name() string {
	return p.name
}

func (p *profileImpl) GetPStates() PStates {
	return p.pstates
}

func (p *profileImpl) GetCStates() CStates {
	return p.cstates
}

// NewPowerProfile creates a new power profile with both P-states and C-states configuration
// C-states can be configured either with explicit names or latency-based filtering
func NewPowerProfile(name string, minFreq, maxFreq uint, governor, epp string, cstates map[string]bool, maxLatencyUs *int) (Profile, error) {
	if !featureList.isFeatureIdSupported(FrequencyScalingFeature) {
		return nil, featureList.getFeatureIdError(FrequencyScalingFeature)
	}

	if err := ValidatePStates(minFreq, maxFreq, 0, 0, governor, epp); err != nil {
		return nil, fmt.Errorf("invalid P-states configuration: %w", err)
	}

	if !featureList.isFeatureIdSupported(CStatesFeature) {
		return nil, featureList.getFeatureIdError(CStatesFeature)
	}

	if err := ValidateCStates(cstates, maxLatencyUs); err != nil {
		return nil, fmt.Errorf("invalid C-states configuration: %w", err)
	}

	log.Info("creating powerProfile object", "name", name)
	return &profileImpl{
		name: name,
		pstates: &pstatesImpl{
			max:          maxFreq * 1000,
			min:          minFreq * 1000,
			efficientMax: maxFreq * 1000,
			efficientMin: minFreq * 1000,
			epp:          epp,
			governor:     governor,
		},
		cstates: cstatesImpl{states: cstates, maxLatencyUs: maxLatencyUs},
	}, nil
}

// creates a Power Profile for efficient and performant cores
// Note: this is not being used by KPM controller, KPM is not supporting E-cores and P-cores
func NewEcorePowerProfile(name string, minFreq, maxFreq, efficientMinFreq, efficientMaxFreq uint, governor, epp string, cstates map[string]bool, maxLatencyUs *int) (Profile, error) {
	if !featureList.isFeatureIdSupported(FrequencyScalingFeature) {
		return nil, featureList.getFeatureIdError(FrequencyScalingFeature)
	}

	if err := ValidatePStates(minFreq, maxFreq, efficientMinFreq, efficientMaxFreq, governor, epp); err != nil {
		return nil, fmt.Errorf("invalid P-states configuration: %w", err)
	}

	if !featureList.isFeatureIdSupported(CStatesFeature) {
		return nil, featureList.getFeatureIdError(CStatesFeature)
	}

	if err := ValidateCStates(cstates, maxLatencyUs); err != nil {
		return nil, fmt.Errorf("invalid C-states configuration: %w", err)
	}

	log.Info("creating powerProfile object", "name", name)
	return &profileImpl{
		name: name,
		pstates: &pstatesImpl{
			max:          maxFreq * 1000,
			min:          minFreq * 1000,
			efficientMax: efficientMaxFreq * 1000,
			efficientMin: efficientMinFreq * 1000,
			epp:          epp,
			governor:     governor,
		},
		cstates: cstatesImpl{states: cstates, maxLatencyUs: maxLatencyUs},
	}, nil
}

func checkGov(governor string) bool {
	for _, element := range availableGovs {
		if element == governor {
			return true
		}
	}
	return false
}

func ValidatePStates(minFreq, maxFreq, eMinFreq, eMaxFreq uint, governor, epp string) error {
	if maxFreq < minFreq {
		return fmt.Errorf("max frequency (%d) cannot be lower than the min frequency (%d)", maxFreq, minFreq)
	}

	if eMaxFreq < eMinFreq {
		return fmt.Errorf("max frequency (%d) cannot be lower than the min frequency (%d)", eMaxFreq, eMinFreq)
	}

	if governor == "" {
		governor = defaultGovernor
	}
	if !checkGov(governor) {
		return fmt.Errorf("governor %s is not supported, please use one of the following: %v", governor, availableGovs)
	}

	if epp != "" && governor == cpuPolicyPerformance && epp != cpuPolicyPerformance {
		return fmt.Errorf("'%s' epp can be used with '%s' governor", cpuPolicyPerformance, cpuPolicyPerformance)
	}

	return nil
}

func ValidateCStates(states map[string]bool, maxLatencyUs *int) error {
	if len(states) > 0 && maxLatencyUs != nil {
		return fmt.Errorf("cannot specify both explicit C-state names and latency-based configuration")
	}

	if maxLatencyUs != nil && *maxLatencyUs < 0 {
		return fmt.Errorf("maxLatencyUs must be a non-negative integer, got %d", *maxLatencyUs)
	} else if len(states) > 0 {
		for name := range states {
			if !slices.Contains(GetAvailableCStates(), name) {
				return fmt.Errorf("c-state %s does not exist on this system", name)
			}
		}
	}

	return nil
}
