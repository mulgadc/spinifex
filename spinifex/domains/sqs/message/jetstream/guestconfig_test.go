package jetstream_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/mulgadc/spinifex/spinifex/domains/sqs/message"
	sqsjs "github.com/mulgadc/spinifex/spinifex/domains/sqs/message/jetstream"
	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nkeys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guestLib is the store guest's shell library, from the repository root.
func guestLib(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	require.True(t, ok)
	p := filepath.Join(filepath.Dir(here), "..", "..", "..", "..", "..", "build", "microvm", "sqs-store", "sqs-store-lib.sh")
	_, err := os.Stat(p)
	require.NoError(t, err)
	return p
}

// guestStore starts a server from the config the store guest itself writes,
// changing only the port, and connects with the client NKey it admits.
func guestStore(t *testing.T) (message.Store, *server.Options) {
	t.Helper()
	kp, err := nkeys.CreateUser()
	require.NoError(t, err)
	pub, err := kp.PublicKey()
	require.NoError(t, err)

	dir := t.TempDir()
	conf := filepath.Join(dir, "nats.conf")
	cmd := exec.Command("bash", "-c", `. "$0" && sqs_write_config 127.0.0.1 "$1" "$2"`, guestLib(t), pub, conf)
	cmd.Env = append(os.Environ(), "SQS_DATA="+filepath.Join(dir, "data"), "SQS_PORT=4222")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "%s", out)

	opts, err := server.ProcessConfigFile(conf)
	require.NoError(t, err)
	opts.Port = -1
	opts.NoLog, opts.NoSigs = true, true
	ns, err := server.NewServer(opts)
	require.NoError(t, err)
	go ns.Start()
	require.True(t, ns.ReadyForConnections(10*time.Second))
	t.Cleanup(ns.Shutdown)

	nc, err := nats.Connect(ns.ClientURL(), nats.Nkey(pub, kp.Sign))
	require.NoError(t, err)
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	require.NoError(t, err)
	ms, err := sqsjs.New(context.Background(), js, sqsjs.Config{MessageStreams: 3, StateBuckets: 2, DuplicateWindow: time.Minute})
	require.NoError(t, err)
	return ms, opts
}

// ADR-0011:S4: the guest's own config admits a full 1 MiB message with the
// metadata a send writes, while the application limit stays at 1 MiB.
func TestS4_GuestConfigAdmitsAFullMessage(t *testing.T) {
	e := newSendEnv(t)
	store, opts := guestStore(t)
	assert.Equal(t, int32(2<<20), opts.MaxPayload)
	assert.True(t, opts.SyncAlways, "the guest syncs every write")

	q := e.create(t, acct, "orders", nil)
	in := input("orders", "")
	in.Body = bytes.Repeat([]byte("a"), message.MaxMessageSize)
	res, err := e.sender(store).Send(context.Background(), in)
	require.NoError(t, err)

	got, err := store.Get(context.Background(), q.Namespace(), res.Position)
	require.NoError(t, err)
	assert.Equal(t, in.Body, got.Body)
	assert.Equal(t, res.MessageID, got.Meta[message.MetaID])
	assert.Equal(t, res.MD5OfBody, got.Meta[message.MetaMD5])

	in.Body = append(in.Body, 'a')
	_, err = e.sender(store).Send(context.Background(), in)
	require.ErrorIs(t, err, message.ErrInvalidParameter)
	u, err := store.Count(context.Background(), q.Namespace())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), u.Messages)
}

// ADR-0011:S4: a full 1 MiB message whose size is shared between ten
// attributes and the body still fits the guest's own payload limit.
func TestS4_GuestConfigAdmitsAFullMessageWithAttributes(t *testing.T) {
	e := newReceiveEnv(t)
	store, _ := guestStore(t)
	q := e.create(t, acct, "orders", nil)

	attrs := []message.Attribute{binAttr("blob", "Binary.raw", bytes.Repeat([]byte{0xff}, 256<<10))}
	for i := range 9 {
		attrs = append(attrs, strAttr("s"+string(rune('0'+i)), "String.label", string(bytes.Repeat([]byte("é"), 1000))))
	}
	set, err := message.NewAttributes(attrs)
	require.NoError(t, err)
	in := input("orders", "")
	in.Body = bytes.Repeat([]byte("a"), message.MaxMessageSize-set.Size())
	in.MessageAttributes = attrs
	res, err := e.sender(store).Send(context.Background(), in)
	require.NoError(t, err)

	got, err := e.receiver(store).Receive(context.Background(), recv("orders", 1))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, in.Body, got[0].Body)
	assert.Equal(t, set.All(), got[0].Attributes.All())
	assert.Equal(t, res.MD5OfMessageAttributes, got[0].MD5OfMessageAttributes)

	in.Body = append(in.Body, 'a')
	_, err = e.sender(store).Send(context.Background(), in)
	require.ErrorIs(t, err, message.ErrInvalidParameter)
	u, err := store.Count(context.Background(), q.Namespace())
	require.NoError(t, err)
	assert.Equal(t, uint64(1), u.Messages)
}
