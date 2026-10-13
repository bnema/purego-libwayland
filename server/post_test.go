package server

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// runDisplay starts a display and stops it at test cleanup.
func runDisplay(t *testing.T) *Display {
	t.Helper()
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	return d
}

func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout waiting for %s", what)
	}
}

// displayTID reports the thread the display goroutine is locked to.
func displayTID(t *testing.T, d *Display) int {
	t.Helper()
	tid := 0
	if !d.Do(func() { tid = unix.Gettid() }) {
		t.Fatal("display stopped")
	}
	return tid
}

func TestPostRunsOnDisplayGoroutine(t *testing.T) {
	d := runDisplay(t)
	want := displayTID(t, d)
	got := make(chan int, 1)
	go d.Post(func() { got <- unix.Gettid() })
	select {
	case tid := <-got:
		if tid != want {
			t.Fatalf("post ran on thread %d, display thread is %d", tid, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post did not run")
	}
}

func TestPostFIFOWithDo(t *testing.T) {
	d := runDisplay(t)
	var order []int
	for i := range 200 {
		d.Post(func() { order = append(order, 2*i) })
		if !d.Do(func() { order = append(order, 2*i+1) }) {
			t.Fatal("display stopped")
		}
	}
	// Do returned, so the display goroutine is done with order.
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d, want %d (%v)", i, v, i, order[:i+1])
		}
	}
	if len(order) != 400 {
		t.Fatalf("%d callbacks ran, want 400", len(order))
	}
}

func TestPostFIFOOrder(t *testing.T) {
	d := runDisplay(t)
	var order []int
	for i := range 1000 {
		d.Post(func() { order = append(order, i) })
	}
	d.Do(func() {})
	for i, v := range order {
		if v != i {
			t.Fatalf("order[%d] = %d", i, v)
		}
	}
	if len(order) != 1000 {
		t.Fatalf("%d posts ran, want 1000", len(order))
	}
}

func TestPostCoalescesWakes(t *testing.T) {
	d := runDisplay(t)
	entered, release := make(chan struct{}), make(chan struct{})
	doDone := make(chan struct{})
	// Hold the display goroutine so every Post lands while one wake is pending.
	go func() { d.Do(func() { close(entered); <-release }); close(doDone) }()
	waitFor(t, entered, "display busy")
	before := d.wakeWrites.Load()
	var mu sync.Mutex
	ran := 0
	for range 1000 {
		d.Post(func() { mu.Lock(); ran++; mu.Unlock() })
	}
	if w := d.wakeWrites.Load() - before; w > 2 {
		t.Fatalf("1000 posts wrote the eventfd %d times", w)
	}
	close(release)
	waitFor(t, doDone, "Do")
	d.Do(func() {})
	mu.Lock()
	defer mu.Unlock()
	if ran != 1000 {
		t.Fatalf("%d posts ran, want 1000", ran)
	}
}

func TestPostAfterStopDropped(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	d.Do(func() {})
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-d.Stopped()
	d.Post(func() { t.Error("post ran after stop") })
	time.Sleep(20 * time.Millisecond)

	// A display closed before Run drops posts too.
	c, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	c.Post(func() { t.Error("post ran after close") })
	c.Post(nil)
}

func TestPostFromPostedFunc(t *testing.T) {
	d := runDisplay(t)
	var order []string
	done := make(chan struct{})
	d.Post(func() {
		order = append(order, "outer")
		d.Post(func() {
			order = append(order, "inner")
			close(done)
		})
		order = append(order, "outer-end")
	})
	waitFor(t, done, "nested post")
	d.Do(func() {})
	want := []string{"outer", "outer-end", "inner"}
	if len(order) != len(want) {
		t.Fatalf("order %v", order)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("order %v, want %v", order, want)
		}
	}
}

// A callback that keeps posting must not stop libwayland from dispatching, and
// the capped rounds must be resumed by a wake instead of waiting for the
// dispatch timeout.
func TestPostSelfRepostingIsCappedAndResumes(t *testing.T) {
	d := runDisplay(t)
	const total = 100 // > maxDrainRounds, so the cap is hit
	var n int
	done := make(chan struct{})
	var step func()
	step = func() {
		n++
		if n == total {
			close(done)
			return
		}
		d.Post(step)
	}
	start := time.Now()
	d.Post(step)
	waitFor(t, done, "self-reposting chain")
	if e := time.Since(start); e > 500*time.Millisecond {
		t.Fatalf("chain of %d posts took %v; wake not re-armed at the round cap", total, e)
	}
	// Do still gets served.
	if !d.Do(func() {}) {
		t.Fatal("Do failed")
	}
}

func TestPostAllocations(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	n := 0
	fn := func() { n++ }
	// Warm up both batch buffers, then drain on this goroutine (not running).
	for range 4 {
		d.Post(fn)
		d.Post(fn)
		d.drain()
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		d.Post(fn)
		d.Post(fn)
		d.drain()
	}); allocs > 0 {
		t.Fatalf("Post+drain allocates %v per run", allocs)
	}
	if n == 0 {
		t.Fatal("posts did not run")
	}
}

func TestAddFDEventfdOnDisplayGoroutine(t *testing.T) {
	d := runDisplay(t)
	want := displayTID(t, d)
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)
	got := make(chan int, 4)
	var src *Source
	if !d.Do(func() {
		src, err = d.AddFD(efd, func() {
			var b [8]byte
			_, _ = unix.Read(efd, b[:])
			got <- unix.Gettid()
		})
	}) || err != nil {
		t.Fatalf("AddFD: %v", err)
	}
	one := [8]byte{1}
	if _, err := unix.Write(efd, one[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case tid := <-got:
		if tid != want {
			t.Fatalf("callback on thread %d, display thread is %d", tid, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("callback did not fire")
	}

	// Remove stops callbacks; it is idempotent.
	d.Do(func() { src.Remove(); src.Remove() })
	if _, err := unix.Write(efd, one[:]); err != nil {
		t.Fatal(err)
	}
	d.Do(func() {})
	time.Sleep(30 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("callback fired after Remove")
	default:
	}
	d.Do(func() {
		if len(live.fds) != 0 {
			t.Errorf("%d fd callbacks leaked", len(live.fds))
		}
	})
}

func TestAddFDTimerfdFiresOnce(t *testing.T) {
	d := runDisplay(t)
	tfd, err := unix.TimerfdCreate(unix.CLOCK_MONOTONIC, unix.TFD_NONBLOCK|unix.TFD_CLOEXEC)
	if err != nil {
		t.Skip(err)
	}
	defer unix.Close(tfd)
	fired := make(chan struct{}, 8)
	if !d.Do(func() {
		_, err = d.AddFD(tfd, func() {
			var b [8]byte
			_, _ = unix.Read(tfd, b[:])
			fired <- struct{}{}
		})
	}) || err != nil {
		t.Fatalf("AddFD: %v", err)
	}
	start := time.Now()
	if err := unix.TimerfdSettime(tfd, 0, &unix.ItimerSpec{Value: unix.NsecToTimespec(int64(time.Millisecond))}, nil); err != nil {
		t.Fatal(err)
	}
	waitFor(t, fired, "timerfd")
	if e := time.Since(start); e > 500*time.Millisecond {
		t.Fatalf("1 ms timer fired after %v", e)
	}
	time.Sleep(50 * time.Millisecond)
	if len(fired) != 0 {
		t.Fatalf("one-shot timer fired %d more times", len(fired))
	}
}

func TestAddFDBeforeRunAndRemoveInCallback(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)
	fired := make(chan struct{}, 4)
	var src *Source
	src, err = d.AddFD(efd, func() {
		var b [8]byte
		_, _ = unix.Read(efd, b[:])
		src.Remove() // from its own callback
		fired <- struct{}{}
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	defer func() { cancel(); <-done }()
	one := [8]byte{1}
	for range 2 {
		if _, err := unix.Write(efd, one[:]); err != nil {
			t.Fatal(err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	waitFor(t, fired, "callback")
	if len(fired) != 0 {
		t.Fatal("callback fired again after removing itself")
	}
}

func TestAddFDErrors(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.AddFD(-1, func() {}); err == nil {
		t.Error("negative fd accepted")
	}
	if _, err := d.AddFD(0, nil); err == nil {
		t.Error("nil callback accepted")
	}
	// A source left registered is freed with the display; Remove is then a no-op.
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)
	src, err := d.AddFD(efd, func() {})
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	src.Remove()
	(*Source)(nil).Remove()
	if _, err := d.AddFD(efd, func() {}); err == nil {
		t.Error("AddFD accepted on a closed display")
	}
	if len(live.fds) != 0 {
		t.Errorf("%d fd callbacks leaked", len(live.fds))
	}
}

// Posts from many goroutines must wake the loop without any Do to rescue a
// lost wake: a lost wake would leave the posts waiting for the 100 ms dispatch
// timeout.
func TestPostMultiProducerNoLostWake(t *testing.T) {
	d := runDisplay(t)
	const producers, perProducer, rounds = 8, 250, 10
	for round := range rounds {
		// Let the loop go idle in epoll_wait.
		time.Sleep(5 * time.Millisecond)
		var left atomic.Int32
		left.Store(producers * perProducer)
		done := make(chan struct{})
		start := time.Now()
		var wg sync.WaitGroup
		for range producers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perProducer {
					d.Post(func() {
						if left.Add(-1) == 0 {
							close(done)
						}
					})
				}
			}()
		}
		waitFor(t, done, "all posts")
		if e := time.Since(start); e > 90*time.Millisecond {
			t.Fatalf("round %d: %d posts took %v; a wake was lost", round, producers*perProducer, e)
		}
		wg.Wait()
	}
}

// Do and Post racing with shutdown must neither panic nor run anything after
// Stopped, and Do's wake must not write to the closed eventfd's number once it
// is reused.
func TestDoPostRaceWithCancel(t *testing.T) {
	for range 20 {
		d, err := NewDisplay()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- d.Run(ctx) }()

		var stopped atomic.Bool
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(2)
			go func() {
				defer wg.Done()
				for range 200 {
					d.Do(func() {
						if stopped.Load() {
							t.Error("Do function ran after Stopped")
						}
					})
				}
			}()
			go func() {
				defer wg.Done()
				for range 200 {
					d.Post(func() {
						if stopped.Load() {
							t.Error("post ran after Stopped")
						}
					})
				}
			}()
		}
		time.Sleep(time.Millisecond)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		<-d.Stopped()
		stopped.Store(true)

		// The closed wake fd's number is free for reuse now. Late Do and Post
		// calls must not write to whoever gets it.
		efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
		if err != nil {
			t.Fatal(err)
		}
		// Force the reuse: make the old wake fd number refer to efd.
		if err := unix.Dup3(efd, d.wake, unix.O_CLOEXEC); err != nil {
			t.Fatal(err)
		}
		for range 50 {
			d.Do(func() { t.Error("Do function ran after Stopped") })
			d.Post(func() { t.Error("post ran after Stopped") })
		}
		wg.Wait()
		var b [8]byte
		if _, err := unix.Read(efd, b[:]); err != unix.EAGAIN {
			t.Fatalf("eventfd %d (display wake fd was %d) was written after stop: %v", efd, d.wake, err)
		}
		unix.Close(efd)
		unix.Close(d.wake)
	}
}

// Sources still registered when Run exits are released with the display.
func TestRunExitReleasesSources(t *testing.T) {
	d, err := NewDisplay()
	if err != nil {
		t.Fatal(err)
	}
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	var addErr error
	if !d.Do(func() { _, addErr = d.AddFD(efd, func() {}) }) || addErr != nil {
		t.Fatalf("AddFD: %v", addErr)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-d.Stopped()
	if len(live.fds) != 0 {
		t.Errorf("%d fd callbacks leaked after Run exited", len(live.fds))
	}
	if len(d.sources) != 0 {
		t.Errorf("%d sources still tracked after Run exited", len(d.sources))
	}
}

func TestPostFromAddFDCallback(t *testing.T) {
	d := runDisplay(t)
	want := displayTID(t, d)
	efd, err := unix.Eventfd(0, unix.EFD_CLOEXEC|unix.EFD_NONBLOCK)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(efd)
	got := make(chan int, 1)
	var src *Source
	if !d.Do(func() {
		src, err = d.AddFD(efd, func() {
			var b [8]byte
			_, _ = unix.Read(efd, b[:])
			d.Post(func() { got <- unix.Gettid() })
		})
	}) || err != nil {
		t.Fatalf("AddFD: %v", err)
	}
	one := [8]byte{1}
	if _, err := unix.Write(efd, one[:]); err != nil {
		t.Fatal(err)
	}
	select {
	case tid := <-got:
		if tid != want {
			t.Fatalf("post ran on thread %d, display thread is %d", tid, want)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("post from AddFD callback did not run")
	}
	d.Do(func() { src.Remove() })
}
