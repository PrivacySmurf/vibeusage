package history

import "github.com/joshuadavidthomas/vibeusage/internal/models"

// PipelineRecorder adapts Append to fetch.Recorder.
type PipelineRecorder struct{}

func (PipelineRecorder) Record(s models.UsageSnapshot) error {
	return Append(s.Provider, s)
}
