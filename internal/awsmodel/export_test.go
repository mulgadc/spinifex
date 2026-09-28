package awsmodel

// Hooks for the external test package.
var ParseSmithyModel = parseSmithyModel

func (m *Model) OperationErrorCodes(operation *Operation) []string {
	return m.operationErrorCodes(operation)
}
