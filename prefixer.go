package torrent

import "fmt"

// Prefixer adds a prefix to another logger's output.
type Prefixer struct {
	Logger Logger
	Prefix string
}

// Info logs an informational message. Arguments are handled in the manner of fmt.Print.
func (p *Prefixer) Info(v ...interface{}) {
	p.Logger.Infof("%s%s", p.Prefix, fmt.Sprint(v...))
}

// Infof logs an informational message. Arguments are handled in the manner of fmt.Print.
func (p *Prefixer) Infof(format string, v ...interface{}) {
	p.Logger.Infof("%s%s", p.Prefix, fmt.Sprintf(format, v...))
}

// Warn logs a warning message. Arguments are handled in the manner of fmt.Print.
func (p *Prefixer) Warn(v ...interface{}) {
	p.Logger.Warnf("%s%s", p.Prefix, fmt.Sprint(v...))
}

// Warnf logs a warning message. Arguments are handled in the manner of fmt.Printf.
func (p *Prefixer) Warnf(format string, v ...interface{}) {
	p.Logger.Warnf("%s%s", p.Prefix, fmt.Sprintf(format, v...))
}

// Error logs an error message. Arguments are handled in the manner of fmt.Print.
func (p *Prefixer) Error(v ...interface{}) {
	p.Logger.Errorf("%s%s", p.Prefix, fmt.Sprint(v...))
}

// Errorf logs an error message. Arguments are handled in the manner of fmt.Printf.
func (p *Prefixer) Errorf(format string, v ...interface{}) {
	p.Logger.Errorf("%s%s", p.Prefix, fmt.Sprintf(format, v...))
}
