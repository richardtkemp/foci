package telegram

import (
	"sync"
	"testing"
)

// #2909: the -race L2 run found SetTyping and refreshTyping reading b.chatID
// without chatMu, while the update loop writes it on every received message.
// Only a -race run can turn this red.
func TestTypingReadsChatIDUnderItsLock(t *testing.T) {
	b, _ := testBot(nil, nil)
	b.SetChatID(1)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int64(2); i < 200; i++ {
			b.SetChatID(i)
		}
	}()
	for i := 0; i < 50; i++ {
		b.SetTyping(true)
		b.refreshTyping()
		b.SetTyping(false)
	}
	wg.Wait()
}
