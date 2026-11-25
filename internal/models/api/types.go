package api

type ReasoningEffort string

const (
	// ReasoningOff disables extended thinking where the model allows it.
	ReasoningOff ReasoningEffort = "off"
	// ReasoningAuto enables thinking but leaves the intensity to the provider
	// default. This is what the legacy boolean `thinking: true` maps to.
	ReasoningAuto ReasoningEffort = "auto"
	// ReasoningMinimal is the weakest graded level.
	ReasoningMinimal ReasoningEffort = "minimal"
	// ReasoningLow is the low graded level.
	ReasoningLow ReasoningEffort = "low"
	// ReasoningMedium is the medium graded level.
	ReasoningMedium ReasoningEffort = "medium"
	// ReasoningHigh is the high graded level.
	ReasoningHigh ReasoningEffort = "high"
	// ReasoningXHigh is the extra-high graded level (OpenAI, Zhipu).
	ReasoningXHigh ReasoningEffort = "xhigh"
	// ReasoningMax is the strongest graded level.
	ReasoningMax ReasoningEffort = "max"
)

type ThinkingLevelMap map[ReasoningEffort]*string

type Message struct {
	Role    string
	Content string
}
