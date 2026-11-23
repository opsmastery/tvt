package report

import (
	"sort"

	"github.com/opsmastery/tvt/pkg/graph"
)

// QueryLoad is the measured query load in the scan's window, by client
// (dashboard, rule, explore, agent, api) and by consumer (S6 item 2,
// version 7): queries logged and the samples (PromQL) and bytes (LogQL)
// they read, as the backends' query logs report them. It shows how much of
// the load agents drive, and which panels and rules cost the most to run.
type QueryLoad struct {
	Queries float64       `json:"queries"`
	Samples float64       `json:"samples"`
	Bytes   float64       `json:"bytes"`
	Clients []ClientLoad  `json:"clients"`
	Top     []QueryRunner `json:"top"`
}

// ClientLoad is one client's share of the load.
type ClientLoad struct {
	Client      string  `json:"client"`
	Queries     float64 `json:"queries"`
	Samples     float64 `json:"samples"`
	Bytes       float64 `json:"bytes"`
	QueryShare  float64 `json:"query_share"`
	SampleShare float64 `json:"sample_share"`
	ByteShare   float64 `json:"byte_share"`
}

// QueryRunner is one consumer whose queries the logs recorded: a panel or
// rule they were matched to, an agent, or an ad hoc query.
type QueryRunner struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Client  string  `json:"client"`
	Queries float64 `json:"queries"`
	Samples float64 `json:"samples"`
	Bytes   float64 `json:"bytes"`
}

// TopQueryRunners is how many consumers QueryLoad.Top lists.
const TopQueryRunners = 10

func queryLoad(consumers map[string]graph.Consumer, costs []graph.QueryCost) QueryLoad {
	ql := QueryLoad{Clients: []ClientLoad{}, Top: []QueryRunner{}}
	byClient := map[string]*ClientLoad{}
	for _, c := range costs {
		con := consumers[c.ConsumerID]
		client := con.Client
		if client == "" {
			client = "unknown"
		}
		cl := byClient[client]
		if cl == nil {
			cl = &ClientLoad{Client: client}
			byClient[client] = cl
		}
		cl.Queries += c.Queries
		cl.Samples += c.Samples
		cl.Bytes += c.Bytes
		ql.Queries += c.Queries
		ql.Samples += c.Samples
		ql.Bytes += c.Bytes
		ql.Top = append(ql.Top, QueryRunner{ID: c.ConsumerID, Name: con.Name, Kind: string(con.Kind), Client: client,
			Queries: c.Queries, Samples: c.Samples, Bytes: c.Bytes})
	}
	share := func(v, total float64) float64 {
		if total <= 0 {
			return 0
		}
		return v / total
	}
	for _, cl := range byClient {
		cl.QueryShare, cl.SampleShare, cl.ByteShare = share(cl.Queries, ql.Queries), share(cl.Samples, ql.Samples), share(cl.Bytes, ql.Bytes)
		ql.Clients = append(ql.Clients, *cl)
	}
	// Ranked by their share of the samples and bytes together, so PromQL
	// and LogQL load compare on one scale.
	weight := func(samples, bytes float64) float64 { return share(samples, ql.Samples) + share(bytes, ql.Bytes) }
	sort.Slice(ql.Clients, func(i, j int) bool {
		a, b := ql.Clients[i], ql.Clients[j]
		if wa, wb := weight(a.Samples, a.Bytes), weight(b.Samples, b.Bytes); wa != wb {
			return wa > wb
		}
		return a.Client < b.Client
	})
	sort.Slice(ql.Top, func(i, j int) bool {
		a, b := ql.Top[i], ql.Top[j]
		if wa, wb := weight(a.Samples, a.Bytes), weight(b.Samples, b.Bytes); wa != wb {
			return wa > wb
		}
		if a.Queries != b.Queries {
			return a.Queries > b.Queries
		}
		return a.ID < b.ID
	})
	if len(ql.Top) > TopQueryRunners {
		ql.Top = ql.Top[:TopQueryRunners]
	}
	return ql
}
