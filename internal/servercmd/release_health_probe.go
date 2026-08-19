package servercmd

import (
	"errors"
	"net/http"
	"time"

	"github.com/YingSuiAI/dirextalk-message-server/p2p"
	"github.com/sirupsen/logrus"
)

// RunReleaseHealthProbe serves only the public build metadata endpoint used by
// the release gate to inspect the exact image under test. It starts no product
// services and accepts connections only from inside the container.
func RunReleaseHealthProbe() {
	server := &http.Server{
		Addr:              "127.0.0.1:18008",
		Handler:           p2p.ReleaseHealthProbeHandler(),
		ReadHeaderTimeout: 2 * time.Second,
	}
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logrus.WithError(err).Fatal("release health probe failed")
	}
}
