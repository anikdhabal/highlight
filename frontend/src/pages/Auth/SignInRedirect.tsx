import { Navigate } from 'react-router-dom'

import { SIGN_IN_ROUTE } from '@/pages/Auth/AuthRouter'
import { authRedirect } from '@/pages/Auth/utils'

export const SignInRedirect: React.FC = () => {
	if (!authRedirect.get()) {
		// Store the original path so we can redirect back to it later.
		authRedirect.set(
			window.location.href.replace(window.location.origin, ''),
		)
	}

	return <Navigate to={SIGN_IN_ROUTE} replace />
}
