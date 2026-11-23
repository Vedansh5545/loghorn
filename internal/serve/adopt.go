package serve

import (
	"github.com/jimbeveridge/loghorn/internal/config"
	"github.com/jimbeveridge/loghorn/internal/incoming"
	"github.com/jimbeveridge/loghorn/internal/recorder"
)

// adopt moves every ended run recorded while serve was down into the store and
// loghorn.log, through a recorder with that run's input settings and service,
// then deletes its folder. A run still writing is left for a later call.
func (s *Server) adopt() {
	list, err := incoming.List(s.o.DataDir)
	if err != nil {
		s.logf("listing incoming runs: %v", err)
		return
	}
	for _, p := range list {
		if p.Locked {
			continue
		}
		in := p.Meta.Input
		if in == (config.Input{}) {
			in = s.o.Config.Input
		}
		rec := recorder.New(recorder.Options{
			Sink: s.sink, Store: s.st, Input: in, Service: p.Meta.Service,
		})
		n := 0
		err := incoming.Adopt(p, func(line []byte) {
			rec.Line(line)
			n++
		}, s.spanFn)
		if err != nil {
			s.logf("adopting run %s: %v", p.Meta.RunID, err)
			continue
		}
		s.logf("adopted run %s (%s, %d records) recorded while serve was down", p.Meta.RunID, p.Meta.Service, n)
	}
}
