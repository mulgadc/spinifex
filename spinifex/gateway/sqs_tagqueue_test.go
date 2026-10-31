package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/mulgadc/spinifex/internal/testkit"
	iamdomain "github.com/mulgadc/spinifex/spinifex/domains/iam"
	sqsawsapi "github.com/mulgadc/spinifex/spinifex/domains/sqs/awsapi"
	"github.com/mulgadc/spinifex/spinifex/domains/sqs/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	tagTarget   = "AmazonSQS.TagQueue"
	untagTarget = "AmazonSQS.UntagQueue"
)

func tagBody(u, tags string) []byte { return []byte(`{"QueueUrl":"` + u + `","Tags":` + tags + `}`) }

func untagBody(u, keys string) []byte {
	return []byte(`{"QueueUrl":"` + u + `","TagKeys":` + keys + `}`)
}

func TestSQSTagQueue_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	u := a.create(t, "orders", nil)
	g := a.record(t, "orders").Generation

	_, err := a.client().TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: aws.String(u), Tags: map[string]string{"team": "a", "env": "dev"}})
	require.NoError(t, err)
	rec := a.record(t, "orders")
	assert.Equal(t, map[string]string{"team": "a", "env": "dev"}, rec.Tags)
	assert.Equal(t, g+1, rec.Generation)

	_, err = a.client().TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: aws.String(u), Tags: map[string]string{"team": "b"}})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "b", "env": "dev"}, a.record(t, "orders").Tags, "overwrites only the named key")

	_, err = a.client().UntagQueue(ctx, &sqs.UntagQueueInput{QueueUrl: aws.String(u), TagKeys: []string{"env", "absent"}})
	require.NoError(t, err)
	rec = a.record(t, "orders")
	assert.Equal(t, map[string]string{"team": "b"}, rec.Tags)
	assert.Equal(t, g+3, rec.Generation)

	// Untagging only absent keys is still an accepted mutation.
	_, err = a.client().UntagQueue(ctx, &sqs.UntagQueueInput{QueueUrl: aws.String(u), TagKeys: []string{"absent"}})
	require.NoError(t, err)
	assert.Equal(t, g+4, a.record(t, "orders").Generation)
}

func TestSQSTagQueue_RequestValidation(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	u := a.create(t, "orders", nil)
	_, err := a.store.Tag(context.Background(), ecrTestAccount, testRegion, "orders", map[string]string{"keep": "1"})
	require.NoError(t, err)
	before := a.record(t, "orders")

	missing := func(member string) wireError {
		return wireError{400, "com.amazon.coral.service#MissingRequiredParameterException", "MissingParameter;Sender",
			"The request must contain the parameter " + member + "."}
	}
	invalid := wireError{400, "com.amazon.coral.service#InvalidParameterValueException", "InvalidParameterValue;Sender", ""}
	badAddr := wireError{404, "com.amazonaws.sqs#InvalidAddress", "InvalidAddress;Sender", ""}
	cases := map[string]struct {
		target string
		body   []byte
		want   wireError
	}{
		"tag: no URL":                   {tagTarget, []byte(`{"Tags":{"a":"1"}}`), missing("QueueUrl")},
		"tag: no tags":                  {tagTarget, []byte(`{"QueueUrl":"` + u + `"}`), missing("Tags")},
		"tag: empty tags":               {tagTarget, tagBody(u, `{}`), missing("Tags")},
		"tag: one reserved among valid": {tagTarget, tagBody(u, `{"ok":"1","aws:x":"2"}`), invalid},
		"tag: empty key":                {tagTarget, tagBody(u, `{"":"1"}`), invalid},
		"tag: wrong host":               {tagTarget, tagBody("http://localhost:1/"+ecrTestAccount+"/orders", `{"a":"1"}`), badAddr},
		"tag: another account":          {tagTarget, tagBody(a.url+"/"+otherOwner+"/orders", `{"a":"1"}`), badAddr},
		"untag: no URL":                 {untagTarget, []byte(`{"TagKeys":["keep"]}`), missing("QueueUrl")},
		"untag: no keys":                {untagTarget, []byte(`{"QueueUrl":"` + u + `"}`), missing("TagKeys")},
		"untag: empty keys":             {untagTarget, untagBody(u, `[]`), missing("TagKeys")},
		"untag: one empty among valid":  {untagTarget, untagBody(u, `["keep",""]`), invalid},
		"untag: another account":        {untagTarget, untagBody(a.url+"/"+otherOwner+"/orders", `["keep"]`), badAddr},
		"untag: trailing input": {untagTarget, append(untagBody(u, `["keep"]`), '}'),
			wireError{400, "com.amazon.coral.service#SerializationException", "MalformedInput;Sender", ""}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := readWireError(t, a.raw(t, tc.target, tc.body))
			want := tc.want
			if want.message == "" {
				want.message = got.message
			}
			assert.Equal(t, want, got)
			assert.Equal(t, before, a.record(t, "orders"), "a rejected request writes nothing")
		})
	}

	t.Run("over the tag limit writes nothing", func(t *testing.T) {
		tags := map[string]string{}
		for i := range 50 {
			tags[string(rune('A'+i/26))+string(rune('a'+i%26))] = "v"
		}
		_, err := a.client().TagQueue(context.Background(), &sqs.TagQueueInput{QueueUrl: aws.String(u), Tags: tags})
		require.Error(t, err, "50 new keys plus the existing one exceed 50")
		assert.Equal(t, before, a.record(t, "orders"))
	})
}

// Another account's same-named queue is never reached through the caller's URL.
func TestSQSTagQueue_AccountBoundary(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	a.seedElsewhere(t, "orders")
	_, err := a.store.Tag(ctx, otherOwner, testRegion, "orders", map[string]string{"team": "theirs"})
	require.NoError(t, err)
	foreign, err := a.store.Resolve(ctx, otherOwner, testRegion, "orders")
	require.NoError(t, err)

	for _, tc := range []struct {
		target string
		body   []byte
	}{{tagTarget, tagBody(a.urlOf("orders"), `{"team":"mine"}`)}, {untagTarget, untagBody(a.urlOf("orders"), `["team"]`)}} {
		got := readWireError(t, a.raw(t, tc.target, tc.body))
		assert.Equal(t, notExist(got), got, tc.target)
	}
	after, err := a.store.Resolve(ctx, otherOwner, testRegion, "orders")
	require.NoError(t, err)
	assert.Equal(t, foreign, after)
}

func TestSQSTagQueue_Authorization(t *testing.T) {
	ordersARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"

	t.Run("each action needs its own grant", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:UntagQueue"}, "*"), true)
		u := a.create(t, "orders", nil)
		before := a.record(t, "orders")
		got := readWireError(t, a.raw(t, tagTarget, tagBody(u, `{"a":"1"}`)))
		assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", got.message}, got)
		assert.Equal(t, before, a.record(t, "orders"))

		b := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:TagQueue"}, "*"), true)
		u = b.create(t, "orders", nil)
		_, err := b.store.Tag(context.Background(), ecrTestAccount, testRegion, "orders", map[string]string{"a": "1"})
		require.NoError(t, err)
		before = b.record(t, "orders")
		got = readWireError(t, b.raw(t, untagTarget, untagBody(u, `["a"]`)))
		assert.Equal(t, 403, got.status)
		assert.Equal(t, before, b.record(t, "orders"))
	})

	t.Run("a grant on another queue does not apply", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:TagQueue", "sqs:UntagQueue"}, "arn:aws:sqs:"+testRegion+":"+ecrTestAccount+":other"), true)
		for _, tc := range []struct {
			target string
			body   []byte
		}{{tagTarget, tagBody(a.urlOf("orders"), `{"a":"1"}`)}, {untagTarget, untagBody(a.urlOf("orders"), `["a"]`)}} {
			assert.Equal(t, 403, readWireError(t, a.raw(t, tc.target, tc.body)).status, tc.target)
		}
	})

	t.Run("denial does not reveal whether the queue exists", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		a.create(t, "orders", nil)
		present := readWireError(t, a.raw(t, tagTarget, tagBody(a.urlOf("orders"), `{"a":"1"}`)))
		absent := readWireError(t, a.raw(t, tagTarget, tagBody(a.urlOf("nope"), `{"a":"1"}`)))
		assert.Equal(t, 403, present.status)
		assert.Equal(t, present.typ, absent.typ)
	})

	t.Run("invalid tags are rejected before authorizing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		got := readWireError(t, a.raw(t, tagTarget, tagBody(a.urlOf("orders"), `{"aws:x":"1"}`)))
		assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
		got = readWireError(t, a.raw(t, untagTarget, untagBody(a.urlOf("orders"), `[""]`)))
		assert.Equal(t, "com.amazon.coral.service#InvalidParameterValueException", got.typ)
	})

	t.Run("queue-scoped grants allow", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:TagQueue", "sqs:UntagQueue"}, ordersARN), true)
		u := a.create(t, "orders", nil)
		assert.Equal(t, http.StatusOK, a.raw(t, tagTarget, tagBody(u, `{"a":"1","b":"2"}`)).StatusCode)
		assert.Equal(t, http.StatusOK, a.raw(t, untagTarget, untagBody(u, `["a"]`)).StatusCode)
		assert.Equal(t, map[string]string{"b": "2"}, a.record(t, "orders").Tags)
	})
}

// Tag condition keys are not enforced by the IAM evaluator, which fails closed:
// a Deny carrying one blocks TagQueue and UntagQueue whatever the tags, and an
// Allow carrying one grants nothing.
func TestSQSTagQueue_TagConditionsFailClosed(t *testing.T) {
	allowAll := iamdomain.Statement{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}}
	type call struct {
		target string
		body   func(u string) []byte
	}
	tag := call{tagTarget, func(u string) []byte { return tagBody(u, `{"team":"a"}`) }}
	untag := call{untagTarget, func(u string) []byte { return untagBody(u, `["team"]`) }}
	cases := []struct {
		name string
		stmt iamdomain.Statement
		call call
	}{
		{"Deny on aws:RequestTag blocks TagQueue", conditioned("Deny", "StringEquals", "aws:RequestTag/team", "blocked", "sqs:TagQueue"), tag},
		{"Deny on aws:TagKeys blocks UntagQueue", conditioned("Deny", "ForAnyValue:StringEquals", "aws:TagKeys", "other", "sqs:UntagQueue"), untag},
		{"Deny on aws:ResourceTag blocks TagQueue", conditioned("Deny", "StringEquals", "aws:ResourceTag/env", "prod", "sqs:TagQueue"), tag},
		{"Deny on aws:ResourceTag blocks UntagQueue", conditioned("Deny", "StringEquals", "aws:ResourceTag/env", "prod", "sqs:UntagQueue"), untag},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{allowAll, tc.stmt}}}
			a := newSQSApp(t, docs, true)
			u := a.create(t, "orders", nil)
			_, err := a.store.Tag(context.Background(), ecrTestAccount, testRegion, "orders", map[string]string{"team": "x"})
			require.NoError(t, err)
			before := a.record(t, "orders")
			assert.Equal(t, 403, readWireError(t, a.raw(t, tc.call.target, tc.call.body(u))).status)
			assert.Equal(t, before, a.record(t, "orders"))
		})
	}

	t.Run("Allow on aws:RequestTag grants nothing", func(t *testing.T) {
		docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{
			{Effect: "Allow", Action: iamdomain.StringOrArr{"sqs:CreateQueue"}, Resource: iamdomain.StringOrArr{"*"}},
			conditioned("Allow", "StringEquals", "aws:RequestTag/team", "a", "sqs:TagQueue"),
		}}}
		a := newSQSApp(t, docs, true)
		u := a.create(t, "orders", nil)
		assert.Equal(t, 403, readWireError(t, a.raw(t, tagTarget, tag.body(u))).status)
		assert.Empty(t, a.record(t, "orders").Tags)
	})
}

// lostTagReplies applies each tag update and then reports it as unavailable.
type lostTagReplies struct{ *queue.Store }

func (q lostTagReplies) Tag(ctx context.Context, account, region, name string, tags map[string]string) (queue.Record, error) {
	if _, err := q.Store.Tag(ctx, account, region, name, tags); err != nil {
		return queue.Record{}, err
	}
	return queue.Record{}, errors.Join(queue.ErrUnavailable, errors.New("reply lost"))
}

func (q lostTagReplies) Untag(ctx context.Context, account, region, name string, keys []string) (queue.Record, error) {
	if _, err := q.Store.Untag(ctx, account, region, name, keys); err != nil {
		return queue.Record{}, err
	}
	return queue.Record{}, errors.Join(queue.ErrUnavailable, errors.New("reply lost"))
}

// An unprovable outcome is 503, never success; a retry is a separate request.
func TestSQSTagQueue_LostReply(t *testing.T) {
	_, _, js := testutil.StartTestJetStream(t)
	store, err := queue.Open(context.Background(), js, 16, 8)
	require.NoError(t, err)
	var app http.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { app.ServeHTTP(w, r) }))
	t.Cleanup(srv.Close)
	_, app = dispatchSeamApp(t, allowAllECRDocs(), sqsawsapi.NewRegistration(sqsawsapi.Deps{Queues: lostTagReplies{store}, Endpoint: srv.URL}))
	a := sqsApp{url: srv.URL, store: store}
	rec, err := store.Create(context.Background(), queue.CreateRequest{Account: ecrTestAccount, Region: testRegion, Name: "orders"})
	require.NoError(t, err)

	unavailable := func(got wireError) wireError {
		return wireError{503, "com.amazon.coral.service#ServiceUnavailableException", "ServiceUnavailable;Receiver", got.message}
	}
	for i, tc := range []struct {
		target string
		body   []byte
		tags   map[string]string
	}{
		{tagTarget, tagBody(a.urlOf("orders"), `{"team":"a"}`), map[string]string{"team": "a"}},
		{tagTarget, tagBody(a.urlOf("orders"), `{"team":"a"}`), map[string]string{"team": "a"}},
		{untagTarget, untagBody(a.urlOf("orders"), `["team"]`), map[string]string{}},
	} {
		got := readWireError(t, a.raw(t, tc.target, tc.body))
		assert.Equal(t, unavailable(got), got, tc.target)
		after := a.record(t, "orders")
		assert.Equal(t, rec.Generation+uint64(i)+1, after.Generation)
		if len(tc.tags) == 0 {
			assert.Empty(t, after.Tags)
		} else {
			assert.Equal(t, tc.tags, after.Tags)
		}
	}
}

const listTagsTarget = "AmazonSQS.ListQueueTags"

func listTagsBody(u string) []byte { return []byte(`{"QueueUrl":"` + u + `"}`) }

func TestSQSListQueueTags_SDK(t *testing.T) {
	a := newSQSApp(t, allowAllECRDocs(), true)
	ctx := context.Background()
	u := a.create(t, "orders", nil)

	out, err := a.client().ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(u)})
	require.NoError(t, err)
	assert.Empty(t, out.Tags, "a queue with no tags lists none")
	resp := a.raw(t, listTagsTarget, listTagsBody(u))
	assert.Equal(t, http.StatusOK, resp.StatusCode)

	_, err = a.client().TagQueue(ctx, &sqs.TagQueueInput{QueueUrl: aws.String(u), Tags: map[string]string{"team": "a", "Env": "Dev"}})
	require.NoError(t, err)
	before := a.record(t, "orders")
	out, err = a.client().ListQueueTags(ctx, &sqs.ListQueueTagsInput{QueueUrl: aws.String(u)})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"team": "a", "Env": "Dev"}, out.Tags)
	assert.Equal(t, before, a.record(t, "orders"), "listing writes nothing")
}

func TestSQSListQueueTags_Authorization(t *testing.T) {
	t.Run("TagQueue does not grant listing", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:TagQueue"}, "*"), true)
		u := a.create(t, "orders", nil)
		got := readWireError(t, a.raw(t, listTagsTarget, listTagsBody(u)))
		assert.Equal(t, wireError{403, "com.amazon.coral.service#AccessDeniedException", "AccessDenied;Sender", got.message}, got)
	})

	t.Run("denial does not reveal whether the queue exists", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue"}, "*"), true)
		a.create(t, "orders", nil)
		present := readWireError(t, a.raw(t, listTagsTarget, listTagsBody(a.urlOf("orders"))))
		absent := readWireError(t, a.raw(t, listTagsTarget, listTagsBody(a.urlOf("nope"))))
		assert.Equal(t, 403, present.status)
		assert.Equal(t, present.typ, absent.typ)
	})

	t.Run("a grant on another queue does not apply", func(t *testing.T) {
		a := newSQSApp(t, sqsPolicy([]string{"sqs:ListQueueTags"}, "arn:aws:sqs:"+testRegion+":"+ecrTestAccount+":other"), true)
		assert.Equal(t, 403, readWireError(t, a.raw(t, listTagsTarget, listTagsBody(a.urlOf("orders")))).status)
	})

	t.Run("Deny on aws:ResourceTag fails closed", func(t *testing.T) {
		allowAll := iamdomain.Statement{Effect: "Allow", Action: iamdomain.StringOrArr{"*"}, Resource: iamdomain.StringOrArr{"*"}}
		docs := []iamdomain.PolicyDocument{{Version: "2012-10-17", Statement: []iamdomain.Statement{
			allowAll, conditioned("Deny", "StringEquals", "aws:ResourceTag/env", "prod", "sqs:ListQueueTags"),
		}}}
		a := newSQSApp(t, docs, true)
		u := a.create(t, "orders", nil)
		assert.Equal(t, 403, readWireError(t, a.raw(t, listTagsTarget, listTagsBody(u))).status)
	})

	t.Run("queue-scoped grant allows", func(t *testing.T) {
		ordersARN := "arn:aws:sqs:" + testRegion + ":" + ecrTestAccount + ":orders"
		a := newSQSApp(t, sqsPolicy([]string{"sqs:CreateQueue", "sqs:ListQueueTags"}, ordersARN), true)
		u := a.create(t, "orders", nil)
		assert.Equal(t, http.StatusOK, a.raw(t, listTagsTarget, listTagsBody(u)).StatusCode)
	})
}
