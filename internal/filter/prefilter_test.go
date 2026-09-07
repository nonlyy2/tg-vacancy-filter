package filter

import "testing"

func TestKeep(t *testing.T) {
	const vacancy = "Ищем Junior Go разработчика в продуктовую команду. " +
		"Стек: Go, PostgreSQL, Docker, Kubernetes. Полностью удалённая работа. " +
		"Зарплата от 400000 тенге. Присылайте резюме в личные сообщения."

	tests := []struct {
		name string
		text string
		keep bool
		why  string
	}{
		{"real vacancy", vacancy, true, ""},
		{"empty", "   ", false, ReasonEmpty},
		{"short reaction", "Отличная вакансия, спасибо!", false, ReasonTooShort},
		{
			name: "long but not hiring",
			text: "Вышла новая версия Go 1.25 с улучшенным сборщиком мусора и " +
				"переработанным пакетом sync. Разбираем, что изменилось в рантайме " +
				"и почему это важно для высоко нагруженных сервисов на практике сегодня.",
			keep: false,
			why:  ReasonNotHiring,
		},
		{
			name: "digest post",
			text: "Все вакансии нашего канала собраны на сайте, переходите по ссылке " +
				"и выбирайте подходящую позицию среди сотен предложений от компаний.",
			keep: false,
			why:  ReasonAggregator,
		},
		{
			name: "english vacancy",
			text: "We are looking for a Junior Backend Engineer to join our team. " +
				"Stack: Python, FastAPI, PostgreSQL. Fully remote position, " +
				"competitive salary. Apply now by sending your CV to our recruiter.",
			keep: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			keep, why := Keep(tt.text)
			if keep != tt.keep {
				t.Fatalf("Keep() = %v, want %v (why=%q)", keep, tt.keep, why)
			}
			if !keep && why != tt.why {
				t.Fatalf("why = %q, want %q", why, tt.why)
			}
		})
	}
}

func TestFingerprintIgnoresRepostNoise(t *testing.T) {
	base := "Ищем Junior Go разработчика. Стек: Go, PostgreSQL. Удалённая работа."

	tests := []struct {
		name  string
		other string
		same  bool
	}{
		{
			name:  "different apply link and channel handle",
			other: base + " Откликнуться: https://t.me/hr_bot @somechannel",
			same:  false, // extra sentence changes the text, not just the link
		},
		{
			name:  "same text, different links only",
			other: "Ищем Junior Go разработчика. Стек: Go, PostgreSQL. Удалённая работа.",
			same:  true,
		},
		{
			name:  "case and whitespace noise",
			other: "ИЩЕМ   Junior  Go разработчика.\n\nСтек: Go, PostgreSQL.  Удалённая работа.",
			same:  true,
		},
		{
			name:  "emoji decoration",
			other: "🔥 Ищем Junior Go разработчика! 🚀 Стек: Go, PostgreSQL. Удалённая работа.",
			same:  true,
		},
		{
			name:  "genuinely different vacancy",
			other: "Ищем Senior Python разработчика. Стек: Django, MySQL. Работа в офисе.",
			same:  false,
		},
	}

	want := Fingerprint(base)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Fingerprint(tt.other)
			if (got == want) != tt.same {
				t.Fatalf("Fingerprint equal = %v, want %v", got == want, tt.same)
			}
		})
	}
}

func TestFingerprintStripsLinksAndHandles(t *testing.T) {
	a := Fingerprint("Ищем Go разработчика удалённо. Пишите https://t.me/recruiter_one @hr_one")
	b := Fingerprint("Ищем Go разработчика удалённо. Пишите https://example.com/apply @hr_two")
	if a != b {
		t.Fatal("links and handles should not affect the fingerprint")
	}
}
