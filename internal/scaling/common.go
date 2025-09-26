package scaling

import (
	"time"

	"github.com/intel/power-optimization-library/pkg/power"
)

const FrequencyNotYetSet int = -1

type CPUScalingOpts struct {
	CPU                        power.Cpu
	SamplePeriod               time.Duration
	CooldownPeriod             time.Duration
	TargetUsage                int
	AllowedUsageDifference     int
	AllowedFrequencyDifference int
	HWMaxFrequency             int
	HWMinFrequency             int
	CurrentTargetFrequency     int
	ScaleFactor                float64
	FallbackFreq               int
}
