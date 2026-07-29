package history

import "github.com/joshuadavidthomas/vibeusage/internal/models"

// PipelineRecorder adapts Append to fetch.Recorder.
type PipelineRecorder struct{}

func (PipelineRecorder) Record(snapshot models.UsageSnapshot) (bool, error) {
	if len(snapshot.Periods) == 0 {
		return false, nil
	}
	if err := Append(snapshot.Provider, snapshot); err != nil {
		return false, err
	}
	return true, nil
}
