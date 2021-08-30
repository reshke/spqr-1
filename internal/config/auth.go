package config

type AuthMethod string

const (
	AuthOK        = AuthMethod("ok")
	AuthNotok     = AuthMethod("notok")
	AuthClearText = AuthMethod("clear_text")
	AuthMD5       = AuthMethod("md5")
	AuthScram      = AuthMethod("scram")
)

type AuthRule struct {
	Method AuthMethod `json:"auth_method" yaml:"auth_method" toml:"auth_method"`
	Password   string     `json:"password" yaml:"password" toml:"password"`
}
