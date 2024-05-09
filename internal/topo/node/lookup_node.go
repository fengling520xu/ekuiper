// Copyright 2021-2024 EMQ Technologies Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package node

import (
	"fmt"

	"github.com/lf-edge/ekuiper/contract/v2/api"
	"github.com/lf-edge/ekuiper/v2/internal/pkg/def"
	"github.com/lf-edge/ekuiper/v2/internal/topo/lookup"
	"github.com/lf-edge/ekuiper/v2/internal/topo/lookup/cache"
	nodeConf "github.com/lf-edge/ekuiper/v2/internal/topo/node/conf"
	"github.com/lf-edge/ekuiper/v2/internal/xsql"
	"github.com/lf-edge/ekuiper/v2/pkg/ast"
	"github.com/lf-edge/ekuiper/v2/pkg/cast"
	"github.com/lf-edge/ekuiper/v2/pkg/infra"
	"github.com/lf-edge/ekuiper/v2/pkg/timex"
)

type LookupConf struct {
	Cache           bool `json:"cache"`
	CacheTTL        int  `json:"cacheTtl"`
	CacheMissingKey bool `json:"cacheMissingKey"`
}

// LookupNode will look up the data from the external source when receiving an event
type LookupNode struct {
	*defaultSinkNode
	sourceType string
	joinType   ast.JoinType
	vals       []ast.Expr

	srcOptions *ast.Options
	conf       *LookupConf
	fields     []string
	keys       []string
}

func NewLookupNode(name string, fields []string, keys []string, joinType ast.JoinType, vals []ast.Expr, srcOptions *ast.Options, options *def.RuleOption) (*LookupNode, error) {
	t := srcOptions.TYPE
	if t == "" {
		return nil, fmt.Errorf("source type is not specified")
	}
	props := nodeConf.GetSourceConf(t, srcOptions)
	lookupConf := &LookupConf{}
	if lc, ok := props["lookup"].(map[string]interface{}); ok {
		err := cast.MapToStruct(lc, lookupConf)
		if err != nil {
			return nil, err
		}
	}
	n := &LookupNode{
		fields:     fields,
		keys:       keys,
		srcOptions: srcOptions,
		conf:       lookupConf,
		sourceType: t,
		joinType:   joinType,
		vals:       vals,
	}
	n.defaultSinkNode = newDefaultSinkNode(name, options)
	return n, nil
}

func (n *LookupNode) Exec(ctx api.StreamContext, errCh chan<- error) {
	log := ctx.GetLogger()
	n.prepareExec(ctx, errCh, "op")
	go func() {
		err := infra.SafeRun(func() error {
			ns, err := lookup.Attach(n.name)
			if err != nil {
				return err
			}
			defer lookup.Detach(n.name)
			fv, _ := xsql.NewFunctionValuersForOp(ctx)
			var c *cache.Cache
			if n.conf.Cache {
				c = cache.NewCache(n.conf.CacheTTL, n.conf.CacheMissingKey)
				defer c.Close()
			}
			// Start the lookup source loop
			for {
				log.Debugf("LookupNode %s is looping", n.name)
				select {
				// process incoming item from both streams(transformed) and tables
				case item := <-n.input:
					data, processed := n.commonIngest(ctx, item)
					if processed {
						break
					}
					n.statManager.IncTotalRecordsIn()
					n.statManager.ProcessTimeStart()
					switch d := data.(type) {
					case xsql.Row:
						log.Debugf("Lookup Node receive tuple input %s", d)
						n.statManager.ProcessTimeStart()
						sets := &xsql.JoinTuples{Content: make([]*xsql.JoinTuple, 0)}
						err := n.lookup(ctx, d, fv, ns, sets, c)
						if err != nil {
							n.Broadcast(err)
							n.statManager.IncTotalExceptions(err.Error())
						} else {
							n.Broadcast(sets)
							n.statManager.IncTotalRecordsOut()
							n.statManager.IncTotalMessagesProcessed(int64(sets.Len()))
						}
						n.statManager.ProcessTimeEnd()
						n.statManager.SetBufferLength(int64(len(n.input)))
					case *xsql.WindowTuples:
						log.Debugf("Lookup Node receive window input %s", d)
						n.statManager.ProcessTimeStart()
						sets := &xsql.JoinTuples{Content: make([]*xsql.JoinTuple, 0), WindowRange: item.(*xsql.WindowTuples).GetWindowRange()}
						err := d.Range(func(i int, r xsql.ReadonlyRow) (bool, error) {
							tr, ok := r.(xsql.Row)
							if !ok {
								return false, fmt.Errorf("Invalid window element, must be a tuple row but got %v", r)
							}
							err := n.lookup(ctx, tr, fv, ns, sets, c)
							if err != nil {
								return false, err
							}
							return true, nil
						})
						if err != nil {
							n.Broadcast(err)
							n.statManager.IncTotalExceptions(err.Error())
						} else {
							n.Broadcast(sets)
							n.statManager.IncTotalRecordsOut()
						}
						n.statManager.ProcessTimeEnd()
						n.statManager.SetBufferLength(int64(len(n.input)))
					default:
						e := fmt.Errorf("run lookup node error: invalid input type but got %[1]T(%[1]v)", d)
						n.Broadcast(e)
						n.statManager.IncTotalExceptions(e.Error())
					}
				case <-ctx.Done():
					log.Info("Cancelling lookup node....")
					return nil
				}
			}
		})
		if err != nil {
			infra.DrainError(ctx, err, errCh)
		}
	}()
}

// lookup will lookup the cache firstly, if expires, read the external source
func (n *LookupNode) lookup(ctx api.StreamContext, d xsql.Row, fv *xsql.FunctionValuer, ns api.LookupSource, tuples *xsql.JoinTuples, c *cache.Cache) error {
	ve := &xsql.ValuerEval{Valuer: xsql.MultiValuer(d, fv)}
	cvs := make([]interface{}, len(n.vals))
	hasNil := false
	for i, val := range n.vals {
		cvs[i] = ve.Eval(val)
		if cvs[i] == nil {
			hasNil = true
		}
	}
	var (
		r  api.SinkTupleList
		e  error
		ok bool
	)
	if !hasNil { // if any of the value is nil, the lookup will always return empty result
		if c != nil {
			k := fmt.Sprintf("%v", cvs)
			r, ok = c.Get(k)
			if !ok {
				r, e = ns.Lookup(ctx, n.fields, n.keys, cvs)
				if e != nil {
					return e
				}
				c.Set(k, r)
			}
		} else {
			r, e = ns.Lookup(ctx, n.fields, n.keys, cvs)
		}
	}
	if e != nil {
		return e
	} else {
		if r != nil && r.Len() == 0 {
			if n.joinType == ast.LEFT_JOIN {
				merged := &xsql.JoinTuple{}
				merged.AddTuple(d)
				tuples.Content = append(tuples.Content, merged)
			} else {
				ctx.GetLogger().Debugf("Lookup Node %s no result found for tuple %s", n.name, d)
				return nil
			}
		}
		r.RangeOfTuples(func(index int, tuple api.MessageTuple) bool {
			merged := &xsql.JoinTuple{}
			merged.AddTuple(d)
			var meta map[string]any
			if mi, ok := tuple.(api.MetaInfo); ok {
				meta = mi.AllMeta()
			}
			t := &xsql.Tuple{
				Emitter:   n.name,
				Message:   tuple.ToMap(),
				Metadata:  meta,
				Timestamp: timex.GetNowInMilli(),
			}
			merged.AddTuple(t)
			tuples.Content = append(tuples.Content, merged)
			return true
		})
		return nil
	}
}

func (n *LookupNode) merge(ctx api.StreamContext, d xsql.Row, r []map[string]interface{}) {
	n.statManager.ProcessTimeStart()
	sets := &xsql.JoinTuples{Content: make([]*xsql.JoinTuple, 0)}

	if len(r) == 0 {
		if n.joinType == ast.LEFT_JOIN {
			merged := &xsql.JoinTuple{}
			merged.AddTuple(d)
			sets.Content = append(sets.Content, merged)
		} else {
			ctx.GetLogger().Debugf("Lookup Node %s no result found for tuple %s", n.name, d)
			return
		}
	}
	for _, v := range r {
		merged := &xsql.JoinTuple{}
		merged.AddTuple(d)
		t := &xsql.Tuple{
			Emitter:   n.name,
			Message:   v,
			Timestamp: timex.GetNowInMilli(),
		}
		merged.AddTuple(t)
		sets.Content = append(sets.Content, merged)
	}

	n.Broadcast(sets)
	n.statManager.ProcessTimeEnd()
	n.statManager.IncTotalRecordsOut()
	n.statManager.IncTotalMessagesProcessed(1)
	n.statManager.SetBufferLength(int64(len(n.input)))
}
