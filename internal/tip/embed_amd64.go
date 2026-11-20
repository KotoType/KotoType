//go:build windows && amd64

package tip

import _ "embed"

//go:embed kototype_tip_x64.dll
var dll64 []byte

//go:embed kototype_tip_x86.dll
var dll32 []byte
