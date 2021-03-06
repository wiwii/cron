package cron

import (
	"log"
	"runtime"
	"sort"
	"time"
)

// Cron keeps track of any number of entries, invoking the associated func as
// specified by the schedule. It may be started, stopped, and the entries may
// be inspected while running.
type Cron struct {
	id       int32
	entries  []*Entry
	stop     chan struct{}
	add      chan *Entry
	snapshot chan []*Entry
	remove    chan int32
	running  bool
	ErrorLog *log.Logger
	location *time.Location
}

// Job is an interface for submitted cron jobs.
type Job interface {
	Run(...interface{})
}

// The Schedule describes a job's duty cycle.
type Schedule interface {
	// Return the next activation time, later than the given time.
	// Next is invoked initially, and then each time the job is run.
	Next(time.Time) time.Time
}

// Entry consists of a schedule and the func to execute on that schedule.
type Entry struct {
	// The schedule on which this job should be run.
	Schedule Schedule

	// The next time the job will run. This is the zero time if Cron has not been
	// started or this entry's schedule is unsatisfiable
	Next time.Time

	// The last time this job was run. This is the zero time if the job has never
	// been run.
	Prev time.Time

	// The Job to run.
	Job Job

	// entry id
	Id int32

	// args
	ArgLen int32

	// tag for running
	Tag string

	//
	Task string

	// 
	Params string
}

// byTime is a wrapper for sorting the entry array by time
// (with zero time at the end).
type byTime []*Entry

func (s byTime) Len() int      { return len(s) }
func (s byTime) Swap(i, j int) { s[i], s[j] = s[j], s[i] }
func (s byTime) Less(i, j int) bool {
	// Two zero times should return false.
	// Otherwise, zero is "greater" than any other time.
	// (To sort it at the end of the list.)
	if s[i].Next.IsZero() {
		return false
	}
	if s[j].Next.IsZero() {
		return true
	}
	return s[i].Next.Before(s[j].Next)
}

// New returns a new Cron job runner, in the Local time zone.
func New() *Cron {
	return NewWithLocation(time.Now().Location())
}

// NewWithLocation returns a new Cron job runner.
func NewWithLocation(location *time.Location) *Cron {
	return &Cron{
		id:       0,
		entries:  nil,
		add:      make(chan *Entry),
		stop:     make(chan struct{}),
		snapshot: make(chan []*Entry),
		remove:   make(chan int32),
		running:  false,
		ErrorLog: nil,
		location: location,
	}
}

// A wrapper that turns a func() into a cron.Job
type FuncJob func(...interface{})

// func (f FuncJob) Run() { f() }

func (f FuncJob) Run(i...interface{}) { f(i...) }

// AddFunc adds a func to the Cron to be run on the given schedule.
func (c *Cron) AddFunc(spec string, cmd func(...interface{})) error {
	return c.AddJob(spec, FuncJob(cmd))
}

func (c *Cron) AddFunc3(spec string, cmd func(...interface{}), n int32) error {
	return c.AddJob(spec, FuncJob(cmd), n)
}

func (c *Cron) AddFunc4(spec string, cmd func(...interface{}), n int32, tag string) error {
	return c.AddJob(spec, FuncJob(cmd), n, tag)
}

func (c *Cron) AddFunc5(spec string, cmd func(...interface{}), n int32, tag string, task string) error {
	return c.AddJob(spec, FuncJob(cmd), n, tag, task)
}

func (c *Cron) AddFunc6(spec string, cmd func(...interface{}), n int32, tag string, task string, params string) error {
	return c.AddJob(spec, FuncJob(cmd), n, tag, task, params)
}

// AddJob adds a Job to the Cron to be run on the given schedule.
func (c *Cron) AddJob(spec string, cmd Job, extArgs ...interface{}) error {
	schedule, err := Parse(spec)
	if err != nil {
		return err
	}
	c.Schedule(schedule, cmd, extArgs)
	return nil
}

// Schedule adds a Job to the Cron to be run on the given schedule.
func (c *Cron) Schedule(schedule Schedule, cmd Job, extArgs ...interface{}) {
	entry := &Entry{
		Schedule: schedule,
		Job:      cmd,
	}
	extArgsInner := extArgs[0].([]interface{})
	switch len(extArgsInner) {
	case 1:
		entry.ArgLen = extArgsInner[0].(int32)
	case 2:
		entry.ArgLen = extArgsInner[0].(int32)
		entry.Tag = extArgsInner[1].(string)
	case 3:
		entry.ArgLen = extArgsInner[0].(int32)
		entry.Tag = extArgsInner[1].(string)
		entry.Task = extArgsInner[2].(string)
	case 4:
		entry.ArgLen = extArgsInner[0].(int32)
		entry.Tag = extArgsInner[1].(string)
		entry.Task = extArgsInner[2].(string)
		entry.Params = extArgsInner[3].(string)	
	}

	if !c.running {
		entry.Id = c.nextId()
		c.entries = append(c.entries, entry)
		return
	}

	c.add <- entry
}

// Entries returns a snapshot of the cron entries.
func (c *Cron) Entries() []*Entry {
	if c.running {
		c.snapshot <- nil
		x := <-c.snapshot
		return x
	}
	return c.entrySnapshot()
}

// Location gets the time zone location
func (c *Cron) Location() *time.Location {
	return c.location
}

// Start the cron scheduler in its own go-routine, or no-op if already started.
func (c *Cron) Start() {
	if c.running {
		return
	}
	c.running = true
	go c.run()
}

// Run the cron scheduler, or no-op if already running.
func (c *Cron) Run() {
	if c.running {
		return
	}
	c.running = true
	c.run()
}

func (c *Cron) runWithRecovery(j Job, args ...interface{}) {
	defer func() {
		if r := recover(); r != nil {
			const size = 64 << 10
			buf := make([]byte, size)
			buf = buf[:runtime.Stack(buf, false)]
			c.logf("cron: panic running job: %v\n%s", r, buf)
		}
	}()
	j.Run(args...)
}

// Run the scheduler. this is private just due to the need to synchronize
// access to the 'running' state variable.
func (c *Cron) run() {
	// Figure out the next activation times for each entry.
	now := c.now()
	for _, entry := range c.entries {
		entry.Next = entry.Schedule.Next(now)
	}

	for {
		// Determine the next entry to run.
		sort.Sort(byTime(c.entries))

		var timer *time.Timer
		if len(c.entries) == 0 || c.entries[0].Next.IsZero() {
			// If there are no entries yet, just sleep - it still handles new entries
			// and stop requests.
			timer = time.NewTimer(100000 * time.Hour)
		} else {
			timer = time.NewTimer(c.entries[0].Next.Sub(now))
		}

		for {
			select {
			case now = <-timer.C:
				now = now.In(c.location)
				// Run every entry whose next time was less than now
				for _, e := range c.entries {
					if e.Next.After(now) || e.Next.IsZero() {
						break
					}
					switch e.ArgLen {
					case 0:
						go c.runWithRecovery(e.Job)
					case 1:
						go c.runWithRecovery(e.Job, e.Id)
					case 2:
						go c.runWithRecovery(e.Job, e.Id, e.Tag)
					case 3:
						go c.runWithRecovery(e.Job, e.Id, e.Tag, e.Task)
					case 4:
						go c.runWithRecovery(e.Job, e.Id, e.Tag, e.Task, e.Params)
					}
					
					e.Prev = e.Next
					e.Next = e.Schedule.Next(now)
				}

			case newEntry := <-c.add:
				timer.Stop()
				now = c.now()
				newEntry.Next = newEntry.Schedule.Next(now)
				newEntry.Id = c.nextId()
				c.entries = append(c.entries, newEntry)

			case <-c.snapshot:
				c.snapshot <- c.entrySnapshot()
				continue

			case targetId := <-c.remove:
				timer.Stop()
				if len(c.entries) <= 0 {
					continue
				}

				if targetId >= 0 {
					newEntrys := []*Entry{}
					for _,v := range c.entries {
						if targetId != v.Id {
							newEntrys = append(newEntrys, v)
						}
					}
					c.entries = newEntrys
				} else if -1 == targetId {
					c.entries = []*Entry{}
				} else if -2 == targetId {
					c.entries = c.entries[1:]
				} else if -3 == targetId {
					c.entries = c.entries[:len(c.entries)-1]
				}

			case <-c.stop:
				timer.Stop()
				return
			}

			break
		}
	}
}

// Logs an error to stderr or to the configured error log
func (c *Cron) logf(format string, args ...interface{}) {
	if c.ErrorLog != nil {
		c.ErrorLog.Printf(format, args...)
	} else {
		log.Printf(format, args...)
	}
}

// Stop stops the cron scheduler if it is running; otherwise it does nothing.
func (c *Cron) Stop() {
	if !c.running {
		return
	}
	c.stop <- struct{}{}
	c.running = false
}

// remove all jobs
func (c *Cron) RemoveAll() {
	if !c.running {
		c.entries = []*Entry{}
		return
	}

	c.remove <- -1
}

// remove spec id
func (c *Cron) Remove(id int32) {
	if !c.running {
		if len(c.entries) <= 0 {
			return
		}

		newEntrys := []*Entry{}
		if id >= 0 {
			for _,v := range c.entries {
				if id != v.Id {
					newEntrys = append(newEntrys, v)
				}
			}
		}
		c.entries = newEntrys
		return
	}

	c.remove <- id
}

// remove top
func (c *Cron) RemoveFirst() {
	if !c.running {
		if len(c.entries) <= 0 {
			return
		}

		c.entries = c.entries[1:]
		return
	}

	c.remove <- -2
}

// remove top
func (c *Cron) RemoveLast() {
	if !c.running {
		if len(c.entries) <= 0 {
			return
		}

		c.entries = c.entries[:len(c.entries)-1]
		return
	}

	c.remove <- -3
}

// entrySnapshot returns a copy of the current cron entry list.
func (c *Cron) entrySnapshot() []*Entry {
	entries := []*Entry{}
	for _, e := range c.entries {
		entries = append(entries, &Entry{
			Schedule: e.Schedule,
			Next:     e.Next,
			Prev:     e.Prev,
			Job:      e.Job,
			Id:       e.Id,
			ArgLen:   e.ArgLen,
			Tag:      e.Tag,
		})
	}
	return entries
}

// now returns current time in c location
func (c *Cron) now() time.Time {
	return time.Now().In(c.location)
}

func (c *Cron) nextId() int32 {
	oid := c.id
	c.id = c.id + 1
	return oid
}
