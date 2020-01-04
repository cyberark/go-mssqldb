package ctxtypes

// ContextKey is, as the name implied, a type reserved
// for keys when passing values into the context
type ContextKey string

const (
	// PreLoginResponseKey is used to obtain PreLogin Response fields
	PreLoginResponseKey ContextKey = "preLoginResponse"
	// ClientLoginKey is used to pass the client Login
	ClientLoginKey ContextKey = "clientLogin"
)

