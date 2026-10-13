package controller

import (
	"testing"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAgentModelMigrationUsesFiveCent25Quotes(t *testing.T) {
	setupDrawingTests(t)
	oldRate := operation_setting.USDExchangeRate
	operation_setting.USDExchangeRate = 1
	t.Cleanup(func() { operation_setting.USDExchangeRate = oldRate })
	require.NoError(t, model.DB.AutoMigrate(&model.AgentCustomerPrice{}))
	_, err := model.UpdateAgentProfile(81001, 1, 5, true, 0, false)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", 81002).Update("inviter_id", 81001).Error)
	require.NoError(t, model.DB.Create(&model.AgentCustomerPrice{CustomerID: 81002, AgentID: 81001, PriceCents: 5}).Error)
	require.NoError(t, model.DB.Create(&model.User{Id: 81003, Username: "ordinary-price", Group: "default", Status: 1}).Error)
	for _, userID := range []int{81001, 81002} {
		quote, err := service.ResolveAgentImageQuote(userID, "gpt-image-2.5")
		require.NoError(t, err)
		require.NotNil(t, quote)
		assert.Equal(t, 5, quote.PriceCents)
		assert.Equal(t, 25000, quote.UnitQuota)
		assert.Equal(t, "gpt-image-2.5", quote.Model)
	}
	for _, name := range []string{"gpt-image-2", "gpt-image-2.5-2k", "gpt-image-2.5-4k"} {
		quote, err := service.ResolveAgentImageQuote(81002, name)
		require.NoError(t, err)
		assert.Nil(t, quote, "agent discount must not spill into legacy or high resolution model %s", name)
	}
	quote, err := service.ResolveAgentImageQuote(81003, "gpt-image-2.5")
	require.NoError(t, err)
	assert.Nil(t, quote, "ordinary customers retain global pricing")
}
