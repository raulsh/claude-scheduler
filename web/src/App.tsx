import { Route, Routes } from 'react-router-dom'

import { Shell } from './components/Shell'
import { Dashboard } from './routes/Dashboard'
import { ExecutionsList } from './routes/ExecutionsList'
import { ExecutionDetail } from './routes/ExecutionDetail'
import { HealthPage } from './routes/HealthPage'
import { TaskForm } from './routes/TaskForm'
import { TaskDetail } from './routes/TaskDetail'
import { NotFound } from './routes/NotFound'

export function App() {
  return (
    <Shell>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="/tasks/new" element={<TaskForm />} />
        <Route path="/tasks/:id" element={<TaskDetail />} />
        <Route path="/tasks/:id/edit" element={<TaskForm />} />
        <Route path="/executions" element={<ExecutionsList />} />
        <Route path="/executions/:id" element={<ExecutionDetail />} />
        <Route path="/health" element={<HealthPage />} />
        <Route path="*" element={<NotFound />} />
      </Routes>
    </Shell>
  )
}
