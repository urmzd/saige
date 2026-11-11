package runtime

// Record names a run writes to durable's ledger. A stored run depends on them,
// so they never change. internal/legacy pins them.
const (
	RecordStop           = stopRecordName
	RecordQuestionPrefix = questionStepPrefix
	RecordAnswerPrefix   = answerRecordPrefix
	RecordChildPrefix    = childStepPrefix
	RecordParkedPrefix   = parkedStepPrefix
	RecordUsagePrefix    = usageRecordName
)
