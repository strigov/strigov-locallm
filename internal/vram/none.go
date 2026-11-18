package vram

import "context"

// none — источник без замеров (§8.3): WaitReleased делает паузу 2 с.
type none struct{}

func (none) Name() string                            { return "none" }
func (none) UsageMiB(context.Context) (int64, error) { return 0, ErrUnavailable }
