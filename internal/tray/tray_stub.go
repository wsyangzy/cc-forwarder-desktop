//go:build stub

package tray

import "context"

type noopController struct{}

func (noopController) Stop() {}

func start(_ context.Context, _ Options) (Controller, error) {
	return noopController{}, nil
}
