// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package plan

import (
	"fmt"
	"io"
	"sync"

	"github.com/SnellerInc/sneller/vm"
)

func (t *Tree) exec(dst vm.QuerySink, ep *ExecParams) error {
	e := mkexec(ep, t.Inputs)
	if err := e.add(dst, &t.Root); err != nil {
		return err
	}
	return e.run()
}

func (n *Node) subexec(ep *ExecParams) error {
	if len(n.Children) == 0 {
		return nil
	}
	e := mkexec(ep, n.Inputs)
	rp := make([]replacement, len(n.Children))
	var wg sync.WaitGroup
	wg.Add(len(n.Children))
	errors := make([]error, len(n.Children))
	for i := range n.Children {
		go func(i int) {
			errors[i] = e.add(&rp[i], n.Children[i])
			wg.Done()
		}(i)
	}
	wg.Wait()
	if err := appenderrs(nil, errors); err != nil {
		return err
	}
	if err := e.run(); err != nil {
		return err
	}
	repl := &replacer{
		inputs: rp,
	}
	n.Op.rewrite(repl)
	return repl.err
}

type task struct {
	input vm.Table
	sink  vm.QuerySink
}

type executor struct {
	ep     *ExecParams
	inputs []Input
	subp   int
	tasks  []task
	extra  []io.Closer // sinks with no inputs
	lock   sync.Mutex
}

func mkexec(ep *ExecParams, inputs []Input) *executor {
	e := &executor{
		ep:     ep,
		inputs: inputs,
		subp:   1,
	}
	if len(inputs) > 0 {
		e.tasks = make([]task, len(inputs))
		e.subp = (ep.Parallel + len(inputs) - 1) / len(inputs)
		if e.subp <= 0 {
			e.subp = 1
		}
	}
	return e
}

func (e *executor) add(dst vm.QuerySink, n *Node) error {
	if err := n.subexec(e.ep); err != nil {
		return err
	}
	in, sink, err := n.Op.wrap(dst, e.ep)
	if err != nil {
		return err
	}
	if sink == nil {
		return nil
	}
	e.lock.Lock()
	defer e.lock.Unlock()
	if in == -1 {
		e.extra = append(e.extra, sink)
		return nil
	}
	if in < 0 || in >= len(e.tasks) {
		return fmt.Errorf("input %d not in plan", in)
	}
	if e.tasks[in].input == nil {
		handle := e.inputs[in].Handle
		if e.ep.Rewrite != nil {
			_, handle = e.ep.Rewrite(e.inputs[in].Table, handle)
		}
		tbl, err := handle.Open(e.ep.Context)
		if err != nil {
			return err
		}
		e.tasks[in].input = tbl
	}
	e.tasks[in].sink = appendSink(e.tasks[in].sink, sink)
	return nil
}

func (e *executor) run() error {
	var wg sync.WaitGroup
	wg.Add(len(e.tasks))
	errors := make([]error, len(e.tasks))
	for i := range e.tasks {
		if e.tasks[i].input == nil {
			wg.Done()
			continue
		}
		go func(i int) {
			t := &e.tasks[i]
			err := t.input.WriteChunks(t.sink, e.subp)
			err2 := t.sink.Close()
			if err == nil {
				err = err2
			}
			errors[i] = err
			e.ep.Stats.observe(t.input)
			wg.Done()
		}(i)
	}
	wg.Wait()
	err := appenderrs(nil, errors)
	for i := range e.extra {
		err = appenderr(err, e.extra[i].Close())
	}
	return err
}
