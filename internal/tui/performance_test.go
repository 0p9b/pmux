package tui

import "testing"

func BenchmarkDashboardRender(b *testing.B) {
	shell := New(nil, cachedFixture(), Options{Plain: true, ReducedMotion: true})
	shell.width, shell.height = 120, 40
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		_ = shell.View()
	}
}

func BenchmarkAllPrimaryScreenRenders(b *testing.B) {
	fixture := cachedFixture()
	for screen := Dashboard; screen <= Logs; screen++ {
		b.Run(screen.String(), func(b *testing.B) {
			shell := New(nil, fixture, Options{Plain: true, ReducedMotion: true, InitialScreen: screen})
			shell.width, shell.height = 120, 40
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				_ = shell.View()
			}
		})
	}
}
