package mail

import (
	"fmt"
	"strings"
)

type tmpl struct{ subject, body string }

// templates[name][locale]. Placeholders use {{key}}. Bodies are plain text (no HTML injection surface).
var templates = map[string]map[string]tmpl{
	"verification_code": {
		"ro": {"Codul tău de verificare: {{code}}", "Bună, {{first_name}}!\n\nCodul tău de verificare este: {{code}}\n\nCodul expiră în {{minutes}} minute și poate fi folosit o singură dată.\nDacă nu ai creat un cont, ignoră acest mesaj.\n"},
		"ru": {"Ваш код подтверждения: {{code}}", "Здравствуйте, {{first_name}}!\n\nВаш код подтверждения: {{code}}\n\nКод действует {{minutes}} минут и может быть использован один раз.\nЕсли вы не создавали аккаунт, проигнорируйте это письмо.\n"},
		"en": {"Your verification code: {{code}}", "Hello {{first_name}},\n\nYour verification code is: {{code}}\n\nIt expires in {{minutes}} minutes and can be used once.\nIf you did not create an account, ignore this email.\n"},
	},
	"email_change_code": {
		"ro": {"Confirmă noua adresă de email: {{code}}", "Codul pentru confirmarea noii adrese este: {{code}}\nExpiră în {{minutes}} minute.\n"},
		"ru": {"Подтвердите новый адрес: {{code}}", "Код для подтверждения нового адреса: {{code}}\nДействует {{minutes}} минут.\n"},
		"en": {"Confirm your new email: {{code}}", "Your code to confirm the new address is: {{code}}\nIt expires in {{minutes}} minutes.\n"},
	},
	"email_change_notice": {
		"ro": {"Schimbare de email solicitată", "S-a solicitat schimbarea adresei de email a contului tău. Dacă nu ai fost tu, schimbă parola imediat.\n"},
		"ru": {"Запрошена смена email", "Запрошена смена адреса электронной почты вашего аккаунта. Если это были не вы, немедленно смените пароль.\n"},
		"en": {"Email change requested", "A change of your account email was requested. If this was not you, change your password immediately.\n"},
	},
	"password_reset": {
		"ro": {"Codul pentru resetarea parolei: {{code}}", "Codul pentru resetarea parolei este: {{code}}\nExpiră în {{minutes}} minute. Dacă nu ai cerut resetarea, ignoră acest mesaj.\n"},
		"ru": {"Код для сброса пароля: {{code}}", "Код для сброса пароля: {{code}}\nДействует {{minutes}} минут. Если вы не запрашивали сброс, проигнорируйте письмо.\n"},
		"en": {"Your password reset code: {{code}}", "Your password reset code is: {{code}}\nIt expires in {{minutes}} minutes. If you did not request it, ignore this email.\n"},
	},
	"account_exists": {
		"ro": {"Încercare de înregistrare", "Cineva a încercat să creeze un cont cu această adresă, dar un cont există deja. Dacă ai uitat parola, folosește opțiunea de resetare.\n"},
		"ru": {"Попытка регистрации", "Кто-то пытался создать аккаунт с этим адресом, но аккаунт уже существует. Если вы забыли пароль, воспользуйтесь восстановлением.\n"},
		"en": {"Registration attempt", "Someone tried to create an account with this address, but one already exists. If you forgot your password, use password reset.\n"},
	},
	"password_changed": {
		"ro": {"Parola a fost schimbată", "Parola contului tău a fost schimbată și sesiunile active au fost închise. Dacă nu ai fost tu, contactează suportul.\n"},
		"ru": {"Пароль изменён", "Пароль вашего аккаунта был изменён, активные сеансы завершены. Если это были не вы, свяжитесь с поддержкой.\n"},
		"en": {"Your password was changed", "Your account password was changed and active sessions were signed out. If this was not you, contact support.\n"},
	},
}

// Render produces subject and plain-text body. Data values are inserted verbatim into text
// emails; they never come from untrusted third parties other than the user's own name.
func Render(name, locale string, data map[string]string) (string, string, error) {
	byLocale, ok := templates[name]
	if !ok {
		return "", "", fmt.Errorf("unknown template %q", name)
	}
	t, ok := byLocale[locale]
	if !ok {
		t = byLocale["ro"]
	}
	repl := make([]string, 0, len(data)*2)
	for k, v := range data {
		repl = append(repl, "{{"+k+"}}", strings.ReplaceAll(v, "\r", ""))
	}
	r := strings.NewReplacer(repl...)
	subject := strings.ReplaceAll(r.Replace(t.subject), "\n", " ")
	return subject, r.Replace(t.body), nil
}
