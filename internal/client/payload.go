package client

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"

	"geminigo/internal/config"
)

func generateUUID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

type ConversationMetadata struct {
	ConversationID string
	ResponseID     string
	ChoiceID       string
}

func BuildPayload(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, xsrfToken string) string {
	return BuildPayloadWithModelAndUUID(prompt, "", modelID, thinkMode, fileRefs, xsrfToken, "")
}

func BuildPayloadWithUUID(prompt string, modelID, thinkMode int, fileRefs []UploadedFile, xsrfToken string, convUUID string) string {
	return BuildPayloadWithModelAndUUID(prompt, "", modelID, thinkMode, fileRefs, xsrfToken, convUUID)
}

func BuildPayloadWithModelAndUUID(prompt string, modelName string, modelID, thinkMode int, fileRefs []UploadedFile, xsrfToken string, convUUID string) string {
	return BuildPayloadWithMetadata(prompt, modelName, modelID, thinkMode, fileRefs, xsrfToken, convUUID, ConversationMetadata{})
}

func BuildPayloadWithMetadata(prompt string, modelName string, modelID, thinkMode int, fileRefs []UploadedFile, xsrfToken string, convUUID string, meta ConversationMetadata) string {
	inner := make([]interface{}, 69)

	// [0]: message content
	if len(fileRefs) > 0 {
		fileData := make([]interface{}, 0, len(fileRefs))
		for _, ref := range fileRefs {
			fileData = append(fileData, []interface{}{[]interface{}{ref.ID}, ref.Name})
		}
		inner[0] = []interface{}{prompt, 0, nil, fileData, nil, nil, 0}
		inner[18] = 1
	} else {
		inner[0] = []interface{}{prompt}
		inner[18] = 0
	}
	inner[1] = []interface{}{"en"}
	inner[2] = []interface{}{meta.ConversationID, meta.ResponseID, meta.ChoiceID, nil, nil, nil, nil, nil, nil, ""}
	if modelName != "" {
		inner[3] = modelName
	} else {
		inner[3] = config.ModelNameForID(modelID) // model name string
	}
	inner[6] = []interface{}{1}
	inner[7] = 1
	inner[10] = 1
	inner[11] = 0
	inner[17] = []interface{}{[]interface{}{thinkMode}}
	inner[27] = 1
	inner[30] = []interface{}{4}
	inner[41] = []interface{}{1}
	inner[53] = 0
	if convUUID != "" {
		inner[59] = convUUID
	} else {
		inner[59] = generateUUID()
	}
	inner[61] = []interface{}{}
	inner[68] = 2

	innerJSON, _ := json.Marshal(inner)
	outer := []interface{}{nil, string(innerJSON)}
	outerJSON, _ := json.Marshal(outer)

	val := url.Values{}
	val.Set("f.req", string(outerJSON))
	
	atToken := config.CONFIG.XsrfToken
	if atToken == "" {
		atToken = xsrfToken
	}
	if atToken != "" {
		val.Set("at", atToken)
	}
	return val.Encode()
}

// ModelHeader returns the x-goog-ext-525001261-jspb value for a given model ID.
func ModelHeader(modelID int) string {
	modelHeaders := map[int]string{
		1:  `[1,null,null,null,"71c2d248d3b102ff"]`,
		2:  `[1,null,null,null,"b37f1b0e57ce22dc"]`,
		3:  `[1,null,null,null,"e6fa609c3fa255c0"]`,
		4:  `[1,null,null,null,"71c2d248d3b102ff"]`,
		5:  `[1,null,null,null,"71c2d248d3b102ff"]`,
		6:  `[1,null,null,null,"71c2d248d3b102ff"]`,
		7:  `[1,null,null,null,"56fdd199312815e2",null,null,0,[4],null,null,2]`,
		8:  `[1,null,null,null,"e051ce1aa80aa576",null,null,0,[4],null,null,2]`,
		10: `[1,null,null,null,"56fdd199312815e2",null,null,0,[4,5,6,8,4,5,6,8],null,null,2,null,null,1,1,"37148B01-D513-431C-B2FD-18F42A6EFE22"]`,
	}
	if v, ok := modelHeaders[modelID]; ok {
		return v
	}
	return `[1,null,null,null,"71c2d248d3b102ff"]`
}
