package cli

import (
	"context"
	"fmt"

	"github.com/smweber/devvm/internal/auth"
)

func (a *App) runAuth(name, tool string) error {
	_, b, err := a.resolve(name)
	if err != nil {
		return err
	}
	tools, err := auth.Tools(tool)
	if err != nil {
		return err
	}
	fmt.Fprintf(a.Stderr, "devvm: starting requested auth flow for '%s' (%s)\n", name, tool)
	return auth.Authenticate(context.Background(), b, tools)
}
