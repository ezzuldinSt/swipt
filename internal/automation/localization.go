package automation

import (
	"encoding/json"

	E "github.com/sagernet/sing/common/exceptions"
	"github.com/swipt-automation/swipt/internal/jsc"
)

func (e *engine) preferredLocalizations(arguments []jsc.Value) (any, error) {
	values, err := texts(arguments, 1)
	if err != nil {
		return nil, err
	}
	var localizations []string
	if err = json.Unmarshal([]byte(values[0]), &localizations); err != nil {
		return nil, E.Cause(err, "decode the localizations")
	}
	return preferredLocalizations(localizations, e.host.options.Languages), nil
}
