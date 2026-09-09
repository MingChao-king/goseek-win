package httpapi

import (
	"testing"
	"time"

	"goseek/internal/domain"
)

// hubEvent 造一个测试用事件。
func hubEvent(sequence int64) domain.RunEvent {
	event := domain.NewEvent("trn_0123456789abcdef0123456789abcdef",
		domain.EventTurnStarted, nil, time.Now())
	event.Sequence = sequence
	return event
}

// 广播要送到每一个订阅者。
func TestHubBroadcastsToAllSubscribers(t *testing.T) {
	hub := NewHub(newDiscardLogger())
	defer hub.Close()

	firstEvents, _, firstDone := hub.Subscribe()
	defer firstDone()
	secondEvents, _, secondDone := hub.Subscribe()
	defer secondDone()

	hub.Emit(hubEvent(1))

	for name, events := range map[string]<-chan domain.RunEvent{"第一个": firstEvents, "第二个": secondEvents} {
		select {
		case event := <-events:
			if event.Sequence != 1 {
				t.Errorf("%s订阅者收到序号 %d", name, event.Sequence)
			}
		case <-time.After(time.Second):
			t.Errorf("%s订阅者没有收到事件", name)
		}
	}
}

// 退订之后不再收到事件。
func TestUnsubscribeStopsDelivery(t *testing.T) {
	hub := NewHub(newDiscardLogger())
	defer hub.Close()

	events, _, unsubscribe := hub.Subscribe()
	unsubscribe()
	hub.Emit(hubEvent(1))

	select {
	case event, ok := <-events:
		if ok {
			t.Errorf("退订后仍收到事件 %+v", event)
		}
	default:
	}
}

// 这是 Hub 最关键的性质：**慢订阅者不能拖住事件的产生方**。
//
// 缓冲写满就丢弃这个订阅者，而不是阻塞——阻塞会让整个 Agent 停在一个卡住的浏览器
// 上。丢连接是安全的：浏览器会自动重连，durable 事件从数据库重放补齐。
func TestSlowSubscriberIsDroppedInsteadOfBlocking(t *testing.T) {
	hub := NewHub(newDiscardLogger())
	defer hub.Close()

	_, dropped, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	// 一直发到超过缓冲深度，全程不读。
	done := make(chan struct{})
	go func() {
		defer close(done)
		for index := range subscriberBuffer + 10 {
			hub.Emit(hubEvent(int64(index + 1)))
		}
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Emit 被慢订阅者阻塞住了")
	}

	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Error("慢订阅者没有被丢弃")
	}
}

// Hub 关闭后，已有订阅者收到结束信号，新订阅立刻结束。
func TestCloseEndsAllSubscriptions(t *testing.T) {
	hub := NewHub(newDiscardLogger())

	_, dropped, unsubscribe := hub.Subscribe()
	defer unsubscribe()

	hub.Close()

	select {
	case <-dropped:
	case <-time.After(time.Second):
		t.Error("关闭后已有订阅者没有收到结束信号")
	}

	_, lateDropped, lateUnsubscribe := hub.Subscribe()
	defer lateUnsubscribe()
	select {
	case <-lateDropped:
	case <-time.After(time.Second):
		t.Error("关闭后的新订阅没有立刻结束")
	}
}

// 关闭之后再发事件不能 panic：Agent 那边可能还有一个 goroutine 正在收尾。
func TestEmitAfterCloseIsSafe(t *testing.T) {
	hub := NewHub(newDiscardLogger())
	hub.Close()
	hub.Emit(hubEvent(1))
}

// 退订可以重复调用：SSE 处理器用 defer 退订，而慢订阅者可能已经被 Hub 摘掉了。
func TestUnsubscribeIsIdempotent(t *testing.T) {
	hub := NewHub(newDiscardLogger())
	defer hub.Close()

	_, _, unsubscribe := hub.Subscribe()
	unsubscribe()
	unsubscribe()
}
