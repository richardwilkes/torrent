package torrent

// Logger defines the API used for logging.
type Logger interface {
	// Info logs an informational message. Arguments are handled in the manner of fmt.Print.
	Info(v ...interface{})
	// Infof logs an informational message. Arguments are handled in the manner of fmt.Print.
	Infof(format string, v ...interface{})
	// Warn logs a warning message. Arguments are handled in the manner of fmt.Print.
	Warn(v ...interface{})
	// Warnf logs a warning message. Arguments are handled in the manner of fmt.Printf.
	Warnf(format string, v ...interface{})
	// Error logs an error message. Arguments are handled in the manner of fmt.Print.
	Error(v ...interface{})
	// Errorf logs an error message. Arguments are handled in the manner of fmt.Printf.
	Errorf(format string, v ...interface{})
}
