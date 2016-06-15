package net_test

import (
	"io"
	"time"

	. "github.com/cloudfoundry/cli/cf/net"
	"github.com/cloudfoundry/cli/cf/net/netfakes"
	"github.com/cloudfoundry/cli/cf/terminal/terminalfakes"
	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

var _ = Describe("ProgressReader", func() {
	var (
		testFile       *netfakes.FakeReadSeekCloser
		progressReader *ProgressReader
		ui             *terminalfakes.FakeUI
		b              []byte
	)

	BeforeEach(func() {
		ui = new(terminalfakes.FakeUI)

		testFile = new(netfakes.FakeReadSeekCloser)

		b = make([]byte, 1024)

		counter := 0
		testFile.ReadStub = func(p []byte) (int, error) {
			counter = counter + 1
			if counter < 2 {
				p = []byte("hello")
				return len(p), nil
			}

			p = []byte("hellohello")
			return len([]byte("hello")), io.EOF
		}

		progressReader = NewProgressReader(testFile, ui, 1*time.Millisecond)
		progressReader.SetTotalSize(int64(len([]byte("hellohello"))))
	})

	It("prints progress while content is being read", func() {
		for {
			time.Sleep(2 * time.Millisecond)
			_, err := progressReader.Read(b)
			if err != nil {
				break
			}
		}

		Eventually(ui.SayCallCount).Should(Equal(1))
		Eventually(func() string {
			output, _ := ui.SayArgsForCall(0)
			return output
		}).Should(ContainSubstring("\rDone "))

		Eventually(ui.PrintCapturingNoOutputCallCount).Should(BeNumerically(">", 0))
		Eventually(func() string {
			output, _ := ui.PrintCapturingNoOutputArgsForCall(0)
			return output
		}).Should(ContainSubstring("uploaded..."))
		Eventually(func() string {
			output, _ := ui.PrintCapturingNoOutputArgsForCall(ui.PrintCapturingNoOutputCallCount() - 1)
			return output
		}).Should(Equal("\r                             "))
	})
})
