import type { SessionState } from '~/types/api'

export function useAppInfo() {
  const { t } = useI18n()
  const session = useState<SessionState>('local-session')
  const isMobile = computed(() => session.value.flags.isMobile)
  const hostKnown = computed(() => session.value.hostKnown)
  const name = computed(() => t('app.name'))
  const description = computed(() =>
    hostKnown.value ? t(isMobile.value ? 'app.mobile' : 'app.desktop') : ''
  )
  return { isMobile, hostKnown, name, description }
}
