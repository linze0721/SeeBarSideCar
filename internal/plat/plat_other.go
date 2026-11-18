//go:build !windows && !darwin

package plat

func New() (Platform, error) { return nil, ErrUnsupported }

// Run calls app on the calling goroutine; other OSes have no UI loop.
func Run(app func()) error {
	app()
	return nil
}
