package awsxml

import (
	"reflect"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshal(t *testing.T) {
	type testStruct struct {
		Name  string `xml:"Name"`
		Value int    `xml:"Value"`
	}

	tests := []struct {
		name        string
		input       any
		expectError bool
		validate    func(t *testing.T, xmlData []byte)
	}{
		{name: "valid struct", input: testStruct{Name: "test", Value: 123}, validate: func(t *testing.T, xmlData []byte) {
			assert.Contains(t, string(xmlData), "<Name>test</Name>")
			assert.Contains(t, string(xmlData), "<Value>123</Value>")
		}},
		{name: "pointer to struct", input: &testStruct{Name: "pointer", Value: 456}, validate: func(t *testing.T, xmlData []byte) {
			assert.Contains(t, string(xmlData), "<Name>pointer</Name>")
			assert.Contains(t, string(xmlData), "<Value>456</Value>")
		}},
		{name: "invalid type", input: make(chan int), expectError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			xmlData, err := Marshal(tt.input)
			if tt.expectError {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
			assert.NotNil(t, xmlData)
			tt.validate(t, xmlData)
		})
	}
}

func TestQueryResponsePayload(t *testing.T) {
	type user struct {
		UserName string `locationName:"UserName" type:"string"`
		UserID   string `locationName:"UserId" type:"string"`
	}

	for _, tt := range []struct {
		name   string
		action string
		user   user
	}{
		{name: "create", action: "CreateUser", user: user{UserName: "testuser", UserID: "AIDA12345"}},
		{name: "list", action: "ListUsers", user: user{UserName: "admin", UserID: "AIDA99999"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			xmlBytes, err := Marshal(QueryResponsePayload(tt.action, tt.user))
			require.NoError(t, err)
			xmlText := string(xmlBytes)
			assert.Contains(t, xmlText, tt.action+"Response")
			assert.Contains(t, xmlText, tt.action+"Result")
			assert.Contains(t, xmlText, tt.user.UserName)
			assert.Contains(t, xmlText, tt.user.UserID)
		})
	}
}

func TestResponsePayload(t *testing.T) {
	type inner struct {
		Name string `locationName:"Name" type:"string"`
	}

	xmlBytes, err := Marshal(ResponsePayload("DescribeInstancesResponse", inner{Name: "test"}))
	require.NoError(t, err)
	xmlText := string(xmlBytes)
	assert.Contains(t, xmlText, "DescribeInstancesResponse")
	assert.Contains(t, xmlText, "test")
}

type normalizeInner struct{ Tags []string }

type normalizeOuter struct {
	Names    []string
	Nested   *normalizeInner
	Children []*normalizeInner
	Value    normalizeInner
}

func TestNormalizeOutput(t *testing.T) {
	t.Run("invalid value", func(t *testing.T) {
		var output any
		assert.Nil(t, NormalizeOutput(output, nil))
	})
	t.Run("top level nil slice", func(t *testing.T) {
		out := NormalizeOutput(normalizeOuter{}, nil).(normalizeOuter)
		require.NotNil(t, out.Names)
		assert.Empty(t, out.Names)
	})
	t.Run("nested pointer", func(t *testing.T) {
		out := NormalizeOutput(normalizeOuter{Nested: &normalizeInner{}}, nil).(normalizeOuter)
		require.NotNil(t, out.Nested)
		require.NotNil(t, out.Nested.Tags)
		assert.Empty(t, out.Nested.Tags)
	})
	t.Run("recurses and preserves configured omissions", func(t *testing.T) {
		asSet := map[reflect.Type][]string{reflect.TypeFor[normalizeInner](): {"Tags"}}
		out := NormalizeOutput(normalizeOuter{
			Nested:   &normalizeInner{},
			Children: []*normalizeInner{{Tags: []string{}}},
		}, asSet).(normalizeOuter)
		assert.Nil(t, out.Nested.Tags)
		require.NotNil(t, out.Children[0].Tags)
		require.NotNil(t, out.Names)
	})
	t.Run("normalizes nested values", func(t *testing.T) {
		out := NormalizeOutput(normalizeOuter{Children: []*normalizeInner{{}, {Tags: []string{"a"}}}}, nil).(normalizeOuter)
		require.NotNil(t, out.Value.Tags)
		require.NotNil(t, out.Children[0].Tags)
		assert.Equal(t, []string{"a"}, out.Children[1].Tags)
	})
	t.Run("leaves nil pointers nil", func(t *testing.T) {
		out := NormalizeOutput(normalizeOuter{}, nil).(normalizeOuter)
		assert.Nil(t, out.Nested)
	})
}

func TestWithRequestID(t *testing.T) {
	type requestIDPayload struct{ Names []string }
	assert.Equal(t, "not-a-struct", WithRequestID("not-a-struct", "req-1"))
	for _, tt := range []struct {
		payload any
		want    []string
	}{
		{payload: requestIDPayload{Names: []string{"a"}}, want: []string{"a"}},
		{payload: &requestIDPayload{Names: []string{"b"}}, want: []string{"b"}},
	} {
		v := reflect.ValueOf(WithRequestID(tt.payload, "req-123"))
		require.Equal(t, "req-123", v.FieldByName("RequestId").String())
		require.Equal(t, tt.want, v.FieldByName("Names").Interface())
	}
}
